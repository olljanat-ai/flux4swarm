// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	"github.com/olljanat-ai/flux4swarm/internal/git"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
	"github.com/olljanat-ai/flux4swarm/internal/kube"
	"github.com/olljanat-ai/flux4swarm/internal/kubebackup"
	"github.com/olljanat-ai/flux4swarm/internal/volumebackup"
)

func newKubeImportCmd() *cobra.Command {
	var (
		gf                  gitFlags
		o                   importer.Options
		ko                  importer.KubeOptions
		kubeconfig, kubeCtx string
		output, exclude     string
		message, author     string
		email, credsDir     string
		secretValues        string
		storageClasses      []string
		watch, validate     bool
		interval            time.Duration
	)
	cmd := &cobra.Command{
		Use:   "kube-import",
		Short: "Export the workloads of a running Kubernetes cluster as Flux manifests to a directory or Git",
		Long: "Read a Kubernetes cluster (kubeconfig: --kubeconfig, KUBECONFIG or\n" +
			"~/.kube/config, --context) and write what was deployed without Flux\n" +
			"(kubectl apply, Helm, operators' own objects) to <path>/apps/<namespace>, in\n" +
			"the layout of `flux4swarm import`. Use it to bring a hand-managed cluster\n" +
			"under GitOps, to keep a Git backup of it (--watch), or to rebuild it on\n" +
			"another cluster: Flux on the new cluster, or `flux4swarm controller` on a\n" +
			"Swarm, applies the export.\n\n" +
			"Everything the cluster fills in (uid, status, managedFields, clusterIP,\n" +
			"volumeName, ...) is removed. Objects with an owner (ReplicaSets, Pods, Jobs of\n" +
			"CronJobs, Secrets of ExternalSecrets), objects Flux applied, and the system\n" +
			"namespaces (kube-*, flux-system, volsync-system) are skipped. Secrets become\n" +
			"ExternalSecrets with one store key per value, so no value reaches Git;\n" +
			"--secret-values writes the values to a local file for loading into the store.\n\n" +
			"With --volsync every claim gets a VolSync ReplicationDestination that restores\n" +
			"the backups `flux4swarm kube-backup` takes on this cluster; with\n" +
			"--volsync-restore-first the target restores every claim before any workload\n" +
			"starts (a migration or disaster recovery).",
		Args: cobra.NoArgs,
	}
	gf.register(cmd, "git-url")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file (default: KUBECONFIG, ~/.kube/config or in-cluster)")
	cmd.Flags().StringVar(&kubeCtx, "context", "", "kubeconfig context (default: current-context)")
	cmd.Flags().StringVar(&o.Cluster, "cluster", "default", "cluster name (writes clusters/<cluster>)")
	cmd.Flags().StringVar(&o.Path, "path", "", "cluster directory in the repository (default clusters/<cluster>)")
	cmd.Flags().StringVar(&output, "output", "", "write to this local directory instead of Git")
	cmd.Flags().BoolVar(&o.Bootstrap, "bootstrap", false, "write flux-system/gotk-sync.yaml so Flux (or the controller) syncs the export")
	cmd.Flags().StringSliceVar(&ko.Namespaces, "namespace", nil, "export only these namespaces (repeatable; system namespaces are exported when named)")
	cmd.Flags().StringVar(&exclude, "exclude", "", "regular expression of objects to skip, matched against namespace/Kind/name (cluster-scoped: Kind/name), e.g. '^monitoring/|/Secret/legacy-'")
	cmd.Flags().BoolVar(&ko.IncludeFluxManaged, "include-flux-managed", false, "also export objects Flux applied (they are in Git already)")
	cmd.Flags().BoolVar(&ko.ClusterScoped, "cluster-scoped", false, "also export cluster-scoped "+strings.Join(importer.ClusterScopedKinds, ", ")+" to <path>/apps/"+importer.ClusterDir)
	cmd.Flags().StringSliceVar(&storageClasses, "storage-class", nil, "rename a storage class in the exported claims, old=new (repeatable), for a target cluster with other storage")
	cmd.Flags().StringVar(&o.SecretStore, "secret-store", "default", "name of the SecretStore ExternalSecrets use")
	cmd.Flags().StringVar(&o.SecretStoreKind, "secret-store-kind", api.KindSecretStore, "SecretStore or ClusterSecretStore")
	cmd.Flags().StringVar(&o.SecretStoreProvider, "secret-store-provider", importer.ProviderVault, "template for the SecretStore: vault, azurekv or fake")
	cmd.Flags().StringVar(&o.SecretKeyPrefix, "secret-key-prefix", "", "prefix for remoteRef.key")
	cmd.Flags().StringVar(&secretValues, "secret-values", "", "write the value of every exported secret key, by store key, to this local JSON file (mode 0600, never to Git) for loading into the store")
	cmd.Flags().BoolVar(&o.VolSync, "volsync", false, "also export, for every claim, a VolSync ReplicationDestination and its repository ExternalSecret that restore the backups of `flux4swarm kube-backup`; workloads that mount a claim are held (0 replicas, Jobs suspended), so the target is a standby")
	cmd.Flags().StringVar(&o.VolSyncSchedule, "volsync-schedule", "", "with --volsync: cron schedule of the restores (default: restore once)")
	cmd.Flags().StringSliceVar(&o.VolSyncEnv, "volsync-env", importer.DefaultVolSyncEnv, "with --volsync: restic env vars, besides RESTIC_REPOSITORY and RESTIC_PASSWORD, that the repository secret carries")
	cmd.Flags().BoolVar(&o.VolSyncRestoreFirst, "volsync-restore-first", false, "implies --volsync, for a migration or a rebuild: write the claims and their one-off restores to <path>/"+importer.DataDir+" and Flux Kustomizations (<path>/"+importer.LayersFile+") that restore every claim first and only then start apps/ at the real replica counts")
	cmd.Flags().StringVar(&message, "commit-message", "", "commit message (default: one written from what changed)")
	cmd.Flags().StringVar(&author, "author", "flux4swarm", "commit author name")
	cmd.Flags().StringVar(&email, "author-email", "flux4swarm@users.noreply.github.com", "commit author email")
	cmd.Flags().StringVar(&credsDir, "credentials-dir", creds.DefaultDir, "directory with the --git-secret credentials")
	cmd.Flags().BoolVar(&validate, "validate", true, "kustomize-build the export like Flux before writing or pushing it")
	cmd.Flags().BoolVar(&watch, "watch", false, "keep running: export again every --interval and commit only when the cluster changed, so Git keeps a versioned backup of a cluster managed by hand")
	cmd.Flags().DurationVar(&interval, "interval", 10*time.Minute, "with --watch: export period")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		if (output == "") == (gf.url == "") {
			return fmt.Errorf("give exactly one of --output or --git-url")
		}
		if secretValues != "" && watch {
			return fmt.Errorf("--secret-values is for a one-off export; it cannot be combined with --watch")
		}
		if exclude != "" {
			re, err := regexp.Compile(exclude)
			if err != nil {
				return fmt.Errorf("--exclude: %w", err)
			}
			ko.Exclude = re
		}
		for _, sc := range storageClasses {
			from, to, ok := strings.Cut(sc, "=")
			if !ok || from == "" || to == "" {
				return fmt.Errorf("--storage-class %q: use old=new", sc)
			}
			if ko.StorageClasses == nil {
				ko.StorageClasses = map[string]string{}
			}
			ko.StorageClasses[from] = to
		}
		ko.SecretValues = secretValues != ""
		o.GitURL, o.Branch, o.GitProvider = gf.url, gf.branch, gf.provider
		if gf.secretRef != "" {
			o.GitSecret = gf.secretRef
		}
		kc, err := kube.New(kubeconfig, kubeCtx)
		if err != nil {
			return err
		}
		store := creds.New(credsDir)
		secretName := gf.apply(store)
		var secret map[string][]byte
		if secretName != "" {
			secret = store.Lookup(secretName, git.SecretKeys...)
		}
		var last string
		export := func(ctx context.Context) (*importer.Result, error) {
			res, err := importer.ExportKubernetes(ctx, kc, o, ko)
			if err != nil {
				return nil, err
			}
			if watch && res.Hash() == last {
				slog.Debug("export unchanged")
				return res, nil
			}
			write := func(root string) error {
				if err := res.WriteTo(root, watch); err != nil {
					return err
				}
				if validate {
					return importer.Validate(root, res.Root)
				}
				return nil
			}
			if output != "" {
				if err := write(output); err != nil {
					return nil, err
				}
				fmt.Printf("wrote %d files to %s\n", len(res.Files), filepath.Join(output, res.Root))
			} else {
				verb := "Import"
				if watch {
					verb = "Sync"
				}
				msg := message
				var describe func([]git.FileChange) string
				if msg == "" {
					msg = verb + " Kubernetes state into " + res.Root
					describe = func(changes []git.FileChange) string { return importer.CommitMessage(verb, changes) }
				}
				work, err := os.MkdirTemp("", "flux4swarm-kube-import-")
				if err != nil {
					return nil, err
				}
				defer os.RemoveAll(work)
				pr, err := git.CommitAndPush(ctx, filepath.Join(work, "repo"), git.PushOptions{
					URL: gf.url, Branch: gf.branch, Message: msg, Describe: describe, Author: author, Email: email,
					Auth:  &git.AuthProvider{Provider: gf.provider, URL: gf.url, Secret: secret},
					Write: write,
				})
				if err != nil {
					return nil, err
				}
				if pr.Changed {
					fmt.Printf("pushed %d files to %s (%s) commit %s\n", len(res.Files), gf.url, gf.branch, pr.Commit)
				} else {
					fmt.Printf("no changes; %s (%s) is up to date\n", gf.url, gf.branch)
				}
			}
			last = res.Hash()
			return res, nil
		}
		if watch {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			slog.Info("watching Kubernetes", "server", kc.Host, "path", o.Path, "interval", interval)
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				if _, err := export(ctx); err != nil {
					slog.Error("export failed; retried at the next interval", "error", err)
				}
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
				}
			}
		}
		res, err := export(ctx)
		if err != nil {
			return err
		}
		if secretValues != "" {
			b, err := json.MarshalIndent(res.SecretValues, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(secretValues, append(b, '\n'), 0o600); err != nil {
				return fmt.Errorf("--secret-values: %w", err)
			}
			fmt.Printf("wrote %d secret values to %s: load them into the store, then delete the file\n", len(res.SecretValues), secretValues)
		}
		for _, sk := range res.Skipped {
			fmt.Println("skipped:", sk)
		}
		for _, n := range res.Notes {
			fmt.Println("note:", strings.TrimSpace(n))
		}
		return nil
	}
	return cmd
}

func newKubeBackupCmd() *cobra.Command {
	var (
		o                   kubebackup.Options
		kubeconfig, kubeCtx string
		env                 []string
		schedule            string
	)
	cmd := &cobra.Command{
		Use:   "kube-backup [NAMESPACE/CLAIM...]",
		Short: "Back up Kubernetes PersistentVolumeClaims with VolSync to restic, for restores on another cluster or a Swarm",
		Long: "Create a VolSync ReplicationSource (restic mover) and its repository Secret\n" +
			"for each claim, on the cluster of the kubeconfig. The data goes to\n" +
			"<repository>/<namespace>/<claim>, the layout volume-backup uses on Swarm, so\n" +
			"the ReplicationDestinations of `kube-import --volsync` restore it on another\n" +
			"Kubernetes cluster and `volume-backup --restore` into a Swarm volume.\n" +
			"VolSync must be installed on this cluster.\n\n" +
			"Without --schedule one backup runs now and is waited for. With --schedule\n" +
			"VolSync keeps backing up in the cluster (incrementally), until\n" +
			"`kube-backup --delete`. For a migration, --final stops the Deployments and\n" +
			"StatefulSets that mount the claims (kube-import exports their replica\n" +
			"counts), then takes the last increment.\n\n" +
			"The repository settings come from RESTIC_REPOSITORY (or --repository),\n" +
			"RESTIC_PASSWORD and the --env variables, or from files named by <NAME>_FILE.",
		Args: cobra.ArbitraryArgs,
	}
	repo, _ := resticEnv("RESTIC_REPOSITORY")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file (default: KUBECONFIG, ~/.kube/config or in-cluster)")
	cmd.Flags().StringVar(&kubeCtx, "context", "", "kubeconfig context (default: current-context)")
	cmd.Flags().BoolVar(&o.All, "all", false, "every bound claim (of --namespace, or of every non-system namespace)")
	cmd.Flags().StringSliceVar(&o.Namespaces, "namespace", nil, "with --all: only these namespaces")
	cmd.Flags().StringVar(&o.Repository, "repository", repo, "base restic repository, e.g. s3:https://s3.example.com/bucket/k8s-prod (env RESTIC_REPOSITORY)")
	cmd.Flags().StringSliceVar(&env, "env", importer.DefaultVolSyncEnv, "environment variables for restic besides RESTIC_PASSWORD (same list as kube-import --volsync-env)")
	cmd.Flags().StringVar(&schedule, "schedule", "", "keep backing up in the cluster: an interval (\"15m\") or a 5-field cron schedule (\"0 1 * * *\")")
	cmd.Flags().StringVar(&o.CopyMethod, "copy-method", kubebackup.CopyDirect, "Direct (any storage; reads the claim as it is), Snapshot or Clone (CSI; a point-in-time copy of a claim in use)")
	cmd.Flags().StringVar(&o.VolumeSnapshotClass, "volume-snapshot-class", "", "with --copy-method Snapshot: VolumeSnapshotClass to use")
	cmd.Flags().IntVar(&o.Keep, "keep", 24, "snapshots restic keeps per claim")
	cmd.Flags().BoolVar(&o.Final, "final", false, "for a migration: stop the workloads that mount the claims (replicas kept in annotation "+api.AnnotationStoppedReplicas+"), then take the last backup")
	cmd.Flags().BoolVar(&o.Wait, "wait", true, "without --schedule: wait until the backups finished")
	cmd.Flags().DurationVar(&o.Timeout, "timeout", 6*time.Hour, "how long to wait for the backups")
	cmd.Flags().BoolVar(&o.Delete, "delete", false, "remove the ReplicationSources and Secrets of kube-backup (stops a --schedule); the restic data stays")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if o.All == (len(args) > 0) {
			return fmt.Errorf("name claims as NAMESPACE/CLAIM or give --all")
		}
		o.Claims = args
		if schedule != "" {
			every, cron, err := volumebackup.ParseSchedule(schedule)
			if err != nil {
				return fmt.Errorf("--schedule: %w", err)
			}
			o.Schedule = cron
			if every > 0 {
				o.Schedule = syncSchedule(every)
			}
		}
		password, _ := resticEnv("RESTIC_PASSWORD")
		o.Env = map[string]string{"RESTIC_PASSWORD": password}
		for _, k := range env {
			if k = strings.TrimSpace(k); k == "" || k == "RESTIC_REPOSITORY" || k == "RESTIC_PASSWORD" {
				continue
			}
			if v, ok := resticEnv(k); ok {
				o.Env[k] = v
			}
		}
		kc, err := kube.New(kubeconfig, kubeCtx)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		res, err := kubebackup.Run(ctx, kc, o)
		for _, r := range res {
			switch {
			case o.Delete:
				fmt.Printf("%s: backup removed (the restic repository is kept)\n", r.Claim)
			case r.Duration != "":
				fmt.Printf("%s: backed up to %s in %s\n", r.Claim, r.Repository, r.Duration)
			default:
				fmt.Printf("%s: backing up to %s (%s)\n", r.Claim, r.Repository, r.Trigger)
			}
			for _, s := range r.Stopped {
				fmt.Printf("%s: stopped %s\n", r.Claim, s)
			}
		}
		return err
	}
	return cmd
}
