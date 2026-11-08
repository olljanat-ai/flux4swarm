// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Command flux4swarm is a Flux compatible GitOps controller for Docker
// Swarm with an import mode for existing Swarms.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/controller"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	"github.com/olljanat-ai/flux4swarm/internal/git"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
	"github.com/olljanat-ai/flux4swarm/internal/merge"
	"github.com/olljanat-ai/flux4swarm/internal/swarm"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// versionInfo returns the version, falling back to the module and VCS
// information Go embeds when the ldflags were not set (go install, go build).
func versionInfo() (v, c, d string) {
	v, c, d = version, commit, date
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && c == "":
				c = s.Value
			case s.Key == "vcs.time" && d == "":
				d = s.Value
			}
		}
	}
	return v, c, d
}

func newVersionCmd() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			v, c, d := versionInfo()
			out := cmd.OutOrStdout()
			if short {
				fmt.Fprintln(out, v)
				return
			}
			fmt.Fprintf(out, "version: %s\n", v)
			if c != "" {
				fmt.Fprintf(out, "commit:  %s\n", c)
			}
			if d != "" {
				fmt.Fprintf(out, "date:    %s\n", d)
			}
			fmt.Fprintf(out, "go:      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print only the version")
	return cmd
}

func main() {
	if err := newRoot().Execute(); err != nil {
		var ex *exitError
		if errors.As(err, &ex) {
			if ex.err != nil {
				fmt.Fprintln(os.Stderr, "error:", ex.err)
			}
			os.Exit(ex.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// exitError exits with code, printing err if it is not nil.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

type globalOpts struct {
	logLevel  string
	logFormat string
	namespace string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newRoot() *cobra.Command {
	g := &globalOpts{}
	root := &cobra.Command{
		Use:           "flux4swarm",
		Short:         "Flux compatible GitOps for Docker Swarm",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := api.SetSwarmNamespace(g.namespace); err != nil {
				return err
			}
			return setupLogging(g)
		},
	}
	root.PersistentFlags().StringVar(&g.logLevel, "log-level", "info", "debug, info, warn or error")
	root.PersistentFlags().StringVar(&g.logFormat, "log-format", "text", "text or json")
	root.PersistentFlags().StringVar(&g.namespace, "default-namespace", envOr(api.EnvSwarmNamespace, api.SwarmNamespace),
		"Kubernetes namespace of Swarm objects without a stack (e.g. made with docker service create): import puts them there and reconcile deploys it without a stack prefix (env "+api.EnvSwarmNamespace+")")
	root.AddCommand(newControllerCmd(modeController), newControllerCmd(modeReconcile), newControllerCmd(modeDiff),
		newImportCmd(), newReportCmd(), newVolumeBackupCmd(), newMergeCmd(), newGetCmd(), newSuspendCmd(true), newSuspendCmd(false), newVersionCmd())
	return root
}

func setupLogging(g *globalOpts) error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(g.logLevel)); err != nil {
		return err
	}
	o := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, o)
	if g.logFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, o)
	}
	slog.SetDefault(slog.New(h))
	return nil
}

// gitFlags are shared by the controller and import commands.
type gitFlags struct {
	url, branch, tag, commit string
	semver                   string
	insecure                 bool
	secretRef, provider      string
	username, password       string
	token                    string
}

func (f *gitFlags) register(cmd *cobra.Command, urlFlag string) {
	cmd.Flags().StringVar(&f.url, urlFlag, "", "Git repository URL (https://github.com/org/repo, https://dev.azure.com/org/project/_git/repo, ssh://..., file://...)")
	cmd.Flags().StringVar(&f.branch, "branch", "main", "Git branch")
	cmd.Flags().StringVar(&f.secretRef, "git-secret", "", "name of the credentials secret (directory in --credentials-dir) for Git")
	cmd.Flags().StringVar(&f.provider, "git-provider", "", "generic (default), github (GitHub App) or azure (Entra ID service principal)")
	cmd.Flags().StringVar(&f.username, "git-username", os.Getenv("GIT_USERNAME"), "Git username (env GIT_USERNAME)")
	cmd.Flags().StringVar(&f.password, "git-password", "", "Git password or personal access token (env GIT_PASSWORD)")
	cmd.Flags().StringVar(&f.token, "git-token", "", "Git bearer token (env GIT_TOKEN)")
	if f.password == "" {
		f.password = os.Getenv("GIT_PASSWORD")
	}
	if f.token == "" {
		f.token = os.Getenv("GIT_TOKEN")
	}
}

// cliSecretName is the in-memory credentials secret filled from flags.
const cliSecretName = "flux4swarm-cli-git"

func (f *gitFlags) apply(store *creds.Store) string {
	if f.password == "" {
		f.password = os.Getenv("GIT_PASSWORD")
	}
	if f.token == "" {
		f.token = os.Getenv("GIT_TOKEN")
	}
	if f.username == "" && f.password == "" && f.token == "" {
		return f.secretRef
	}
	if f.username != "" {
		store.Set(cliSecretName, git.KeyUsername, []byte(f.username))
	}
	if f.password != "" {
		store.Set(cliSecretName, git.KeyPassword, []byte(f.password))
	}
	if f.token != "" {
		store.Set(cliSecretName, git.KeyBearerToken, []byte(f.token))
	}
	return cliSecretName
}

func dockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

const (
	modeController = "controller"
	modeReconcile  = "reconcile"
	modeDiff       = "diff"
)

func newControllerCmd(mode string) *cobra.Command {
	once := mode != modeController
	var (
		gf          gitFlags
		configs     []string
		path        string
		interval    time.Duration
		ksInterval  time.Duration
		timeout     time.Duration
		prune, wait bool
		dryRun      bool
		strict      bool
		credsDir    string
		cacheDir    string
		listen      string
		receiver    string
		registryCfg string
		apiToken    string
	)
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the GitOps controller (pull Git, apply to Swarm, prune, repeat)",
		Args:  cobra.NoArgs,
	}
	switch mode {
	case modeReconcile:
		cmd.Use = "reconcile"
		cmd.Short = "Reconcile once and exit (non-zero if anything is not Ready)"
	case modeDiff:
		cmd.Use = "diff"
		cmd.Short = "Show a unified diff of what a reconcile would change in Swarm, without changing it"
		cmd.Long = "Build the source like reconcile --dry-run and print a unified diff of every\n" +
			"service and config that would change, plus the objects that would be created\n" +
			"or deleted. Secret values are never shown.\n\n" +
			"Exit status: 0 no differences, 1 differences found, 2 error (like flux diff)."
	}
	gf.register(cmd, "url")
	cmd.Flags().StringVar(&gf.tag, "tag", "", "Git or OCI tag (instead of --branch)")
	cmd.Flags().StringVar(&gf.semver, "semver", "", "semver range of the Git or OCI tag to use, e.g. '>=1.0.0 <2.0.0' (instead of --branch/--tag)")
	cmd.Flags().StringVar(&gf.commit, "commit", "", "Git commit SHA")
	cmd.Flags().BoolVar(&gf.insecure, "insecure", false, "use plain HTTP for an oci:// --url")
	cmd.Flags().StringArrayVar(&configs, "config", nil, "file with GitRepository and Kustomization objects (repeatable), e.g. clusters/prod/flux-system/gotk-sync.yaml")
	cmd.Flags().StringVar(&path, "path", "", "path in the repository to apply; without --url a local directory")
	cmd.Flags().DurationVar(&interval, "interval", time.Minute, "source fetch interval")
	cmd.Flags().DurationVar(&ksInterval, "kustomization-interval", 10*time.Minute, "Kustomization reconcile interval")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "apply and health check timeout")
	cmd.Flags().BoolVar(&prune, "prune", true, "delete Swarm objects removed from the source")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for services to converge")
	if mode != modeDiff {
		cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change without changing Swarm")
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "fail a Kustomization that uses Kubernetes features Swarm does not support, instead of skipping them with a warning")
	cmd.Flags().StringVar(&credsDir, "credentials-dir", creds.DefaultDir, "directory with credential secrets (<dir>/<name>/<key> or <dir>/<name>_<key>)")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", filepath.Join(os.TempDir(), "flux4swarm"), "directory for Git checkouts")
	if !once {
		cmd.Flags().StringVar(&listen, "listen", ":9797", "address for /healthz, /readyz, /status, /metrics and POST /reconcile, /suspend and /resume")
		cmd.Flags().StringVar(&receiver, "receiver-listen", ":9292", "address for Flux Receiver webhooks (/hook/...); empty disables them")
		cmd.Flags().StringVar(&apiToken, "api-token", "", "bearer token required by the POST endpoints (env FLUX4SWARM_API_TOKEN)")
	}
	cmd.Flags().StringVar(&registryCfg, "registry-config", defaultRegistryConfig(), "Docker config.json with registry credentials")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		dc, err := dockerClient()
		if err != nil {
			return err
		}
		defer dc.Close()
		store := creds.New(credsDir)
		auths, err := swarm.LoadDockerConfig(registryCfg)
		if err != nil {
			return err
		}
		if apiToken == "" {
			apiToken = os.Getenv("FLUX4SWARM_API_TOKEN")
		}
		host, _ := os.Hostname()
		c := controller.New(controller.Options{
			Docker: dc, Creds: store, CacheDir: cacheDir, RegistryAuth: auths, DryRun: dryRun,
			Diff: mode == modeDiff, Strict: strict, APIToken: apiToken, Log: slog.Default(),
			Instance: host,
		})
		if err := addObjects(c, store, &gf, configs, path, interval, ksInterval, timeout, prune, wait); err != nil {
			return err
		}
		if mode == modeDiff {
			err := c.ReconcileOnce(ctx)
			drift := printDiff(os.Stdout, c.Status())
			if err != nil {
				return &exitError{code: 2, err: err}
			}
			if drift {
				return &exitError{code: 1}
			}
			return nil
		}
		if once {
			err := c.ReconcileOnce(ctx)
			printStatus(c.Status())
			return err
		}
		srv := &http.Server{Addr: listen, Handler: c.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("status server", "error", err)
			}
		}()
		var hooks *http.Server
		if receiver != "" {
			hooks = &http.Server{Addr: receiver, Handler: c.ReceiverHandler(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				if err := hooks.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Error("receiver server", "error", err)
				}
			}()
		}
		slog.Info("controller started", "version", version, "commit", commit, "listen", listen, "receiver-listen", receiver)
		err = c.Run(ctx)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		if hooks != nil {
			_ = hooks.Shutdown(shutdown)
		}
		return err
	}
	return cmd
}

func addObjects(c *controller.Controller, store *creds.Store, gf *gitFlags, configs []string, path string,
	interval, ksInterval, timeout time.Duration, prune, wait bool) error {
	if len(configs) == 0 && gf.url == "" && path == "" {
		return fmt.Errorf("give --url (and --path), --path with a local directory, or --config")
	}
	for _, f := range configs {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		set := &api.Set{}
		if err := set.Decode(f, b, api.DefaultNamespace); err != nil {
			return err
		}
		if len(set.GitRepositories)+len(set.OCIRepositories)+len(set.Kustomizations)+len(set.Providers)+len(set.Alerts)+len(set.Receivers) == 0 {
			return fmt.Errorf("%s has no GitRepository, OCIRepository, Kustomization, Provider, Alert or Receiver", f)
		}
		// Providers first, so Alerts find them.
		for _, p := range set.Providers {
			_ = c.AddNotification(p)
		}
		for _, r := range set.Receivers {
			_ = c.AddNotification(r)
		}
		for _, a := range set.Alerts {
			_ = c.AddNotification(a)
		}
		for _, r := range set.GitRepositories {
			c.AddGitRepository(r)
		}
		for _, r := range set.OCIRepositories {
			c.AddOCIRepository(r)
		}
		for _, k := range set.Kustomizations {
			c.AddKustomization(k)
		}
	}
	if gf.url == "" && path == "" {
		return nil
	}
	to := api.Duration{Duration: timeout}
	ks := &api.Kustomization{
		TypeMeta: api.TypeMeta{APIVersion: api.KustomizeGroupVersion, Kind: api.KindKustomization},
		Metadata: api.ObjectMeta{Name: "flux-system", Namespace: api.DefaultNamespace},
		Spec: api.KustomizationSpec{
			Interval: api.Duration{Duration: ksInterval},
			Prune:    prune,
			Wait:     wait,
			Timeout:  &to,
		},
	}
	if gf.url == "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return fmt.Errorf("--path %s is not a directory", path)
		}
		c.AddLocalSource("local", abs)
		ks.Spec.Path = "./"
		ks.Spec.SourceRef = api.CrossNamespaceObjectReference{Kind: controller.LocalSourceKind, Name: "local"}
		c.AddKustomization(ks)
		return nil
	}
	if path == "" {
		path = "./"
	}
	ks.Spec.Path = path
	var secretRef *api.LocalObjectReference
	if name := gf.apply(store); name != "" {
		secretRef = &api.LocalObjectReference{Name: name}
	}
	if strings.HasPrefix(gf.url, "oci://") {
		or := &api.OCIRepository{
			TypeMeta: api.TypeMeta{APIVersion: api.SourceGroupVersion, Kind: api.KindOCIRepository},
			Metadata: api.ObjectMeta{Name: "flux-system", Namespace: api.DefaultNamespace},
			Spec: api.OCIRepositorySpec{
				URL:       gf.url,
				Interval:  api.Duration{Duration: interval},
				Insecure:  gf.insecure,
				SecretRef: secretRef,
				Ref:       &api.OCIRepositoryRef{Tag: gf.tag, SemVer: gf.semver},
			},
		}
		c.AddOCIRepository(or)
		ks.Spec.SourceRef = api.CrossNamespaceObjectReference{Kind: api.KindOCIRepository, Name: "flux-system"}
		c.AddKustomization(ks)
		return nil
	}
	gr := &api.GitRepository{
		TypeMeta: api.TypeMeta{APIVersion: api.SourceGroupVersion, Kind: api.KindGitRepository},
		Metadata: api.ObjectMeta{Name: "flux-system", Namespace: api.DefaultNamespace},
		Spec: api.GitRepositorySpec{
			URL:       gf.url,
			Interval:  api.Duration{Duration: interval},
			Provider:  gf.provider,
			Ref:       &api.GitRepositoryRef{Branch: gf.branch, Tag: gf.tag, SemVer: gf.semver, Commit: gf.commit},
			SecretRef: secretRef,
		},
	}
	if gf.tag != "" || gf.semver != "" {
		gr.Spec.Ref.Branch = ""
	}
	c.AddGitRepository(gr)
	ks.Spec.SourceRef = api.CrossNamespaceObjectReference{Kind: api.KindGitRepository, Name: "flux-system"}
	c.AddKustomization(ks)
	return nil
}

func printStatus(st []controller.ObjectStatus) {
	for _, s := range st {
		fmt.Printf("%s/%s/%s\tready=%s\t%s\n", s.Kind, s.Namespace, s.Name, s.Ready.Status, s.Ready.Message)
		for _, ch := range s.LastChanges {
			if ch.Action != swarm.Unchanged {
				fmt.Printf("  %s\n", ch)
			}
		}
		for _, w := range s.Warnings {
			fmt.Printf("  warning: %s\n", w)
		}
		for _, ig := range s.Skipped {
			fmt.Printf("  skipped: %s (%s)\n", ig.Object, ig.Reason)
		}
	}
}

// printDiff prints the changes a dry run with Diff recorded, in the style
// of `flux diff`, and reports whether anything would change.
func printDiff(w io.Writer, st []controller.ObjectStatus) bool {
	drift := false
	for _, s := range st {
		if s.Kind != api.KindKustomization {
			continue
		}
		var changed []swarm.Change
		for _, ch := range s.LastChanges {
			if ch.Action != swarm.Unchanged {
				changed = append(changed, ch)
			}
		}
		if len(changed) == 0 && len(s.Warnings) == 0 && len(s.Skipped) == 0 {
			continue
		}
		fmt.Fprintf(w, "Kustomization %s/%s:\n", s.Namespace, s.Name)
		for _, ch := range changed {
			drift = true
			verb := string(ch.Action)
			if ch.Action == swarm.Configured {
				verb = "drifted"
			}
			line := fmt.Sprintf("► %s/%s %s", ch.Kind, ch.Name, verb)
			if ch.Reason != "" {
				line += " (" + ch.Reason + ")"
			}
			fmt.Fprintln(w, line)
			if ch.Diff != "" {
				fmt.Fprint(w, ch.Diff)
			}
		}
		for _, warn := range s.Warnings {
			fmt.Fprintf(w, "  warning: %s\n", warn)
		}
		for _, ig := range s.Skipped {
			fmt.Fprintf(w, "  skipped: %s (%s)\n", ig.Object, ig.Reason)
		}
	}
	return drift
}

func defaultRegistryConfig() string {
	if v := os.Getenv("DOCKER_CONFIG"); v != "" {
		return filepath.Join(v, "config.json")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".docker", "config.json")
	}
	return ""
}

func newImportCmd() *cobra.Command {
	var (
		gf       gitFlags
		o        importer.Options
		output   string
		exclude  string
		message  string
		author   string
		email    string
		credsDir string

		adoptOwner     string
		watch          bool
		validate       bool
		interval       time.Duration
		debounce       time.Duration
		listen         string
		unhealthyAfter time.Duration
		mergeEnvs      []string
		basePath       string
	)
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Export Swarm services, configs, secrets and volumes as Flux/Kubernetes manifests to a directory or Git",
		Long: "Export the Swarm once, or with --watch keep exporting it: Swarm stays the\n" +
			"source of truth and Flux on Kubernetes follows the commits. In watch mode\n" +
			"the export owns only <path>/apps and <path>/MIGRATION.md; files elsewhere (flux-system/, your own\n" +
			"overlays and patches) are created once when missing and never overwritten.\n\n" +
			"With --merge, every export also regenerates the shared Kustomize base\n" +
			"(--base-path) and the overlay clusters/ENV/apps of each listed environment\n" +
			"already in the repository, as \"flux4swarm merge\" does. Run one watcher per\n" +
			"Swarm with --cluster set to its environment and the same --merge list\n" +
			"everywhere; a push that loses a race with another watcher is retried on\n" +
			"the new tip.",
		Args: cobra.NoArgs,
	}
	gf.register(cmd, "git-url")
	cmd.Flags().StringVar(&o.Cluster, "cluster", "default", "cluster name (writes clusters/<cluster>)")
	cmd.Flags().StringVar(&o.Path, "path", "", "cluster directory in the repository (default clusters/<cluster>)")
	cmd.Flags().StringVar(&output, "output", "", "write to this local directory instead of Git")
	cmd.Flags().BoolVar(&o.Bootstrap, "bootstrap", false, "write flux-system/gotk-sync.yaml so the controller (or Flux) syncs the export")
	cmd.Flags().BoolVar(&o.Adopt, "adopt", false, "keep the image digests running now and label the exported services as owned, so the first reconcile changes nothing")
	cmd.Flags().StringVar(&adoptOwner, "adopt-owner", "flux-system", "with --adopt: name of the Kustomization (in namespace flux-system) that takes ownership")
	cmd.Flags().StringVar(&o.SecretStore, "secret-store", "default", "name of the SecretStore ExternalSecrets use")
	cmd.Flags().StringVar(&o.SecretStoreKind, "secret-store-kind", api.KindSecretStore, "SecretStore or ClusterSecretStore")
	cmd.Flags().StringVar(&o.SecretStoreProvider, "secret-store-provider", importer.ProviderVault, "template for the SecretStore: vault, azurekv or fake")
	cmd.Flags().StringVar(&o.SecretKeyPrefix, "secret-key-prefix", "", "prefix for remoteRef.key")
	cmd.Flags().StringVar(&exclude, "exclude", "", "regular expression of Swarm object names to skip")
	cmd.Flags().StringSliceVar(&o.SecretEnvNames, "secret-env-name", nil, "extra env var name part (case-insensitive) whose values go to the secret store instead of Git; repeatable, added to the built-in list ("+strings.Join(importer.DefaultSecretEnvNames, ", ")+")")
	cmd.Flags().BoolVar(&o.VolSync, "volsync", false, "also export, for every PersistentVolumeClaim, a VolSync ReplicationDestination (restic mover) and its repository ExternalSecret, so Kubernetes restores the data copied by `flux4swarm volume-backup` (needs VolSync on the cluster)")
	cmd.Flags().StringSliceVar(&o.VolSyncEnv, "volsync-env", importer.DefaultVolSyncEnv, "with --volsync: restic env vars, besides RESTIC_REPOSITORY and RESTIC_PASSWORD, that the repository secret carries")
	cmd.Flags().BoolVar(&o.NoDefaultSecretEnvNames, "no-default-secret-env-names", false, "do not use the built-in --secret-env-name list; with no --secret-env-name, all env values are exported as is")
	cmd.Flags().StringVar(&message, "commit-message", "", "commit message (default: one written from what changed, such as \"shop: api image 1.4→1.5, removed tmp-debug\")")
	cmd.Flags().StringVar(&author, "author", "flux4swarm", "commit author name")
	cmd.Flags().StringVar(&email, "author-email", "flux4swarm@users.noreply.github.com", "commit author email")
	cmd.Flags().StringVar(&credsDir, "credentials-dir", creds.DefaultDir, "directory with the --git-secret credentials")
	cmd.Flags().BoolVar(&watch, "watch", false, "keep running: export again after every Swarm service, config or secret change and every --interval, and commit only when the export differs (Swarm stays the source of truth); with --merge, one watcher per Swarm can share a base")
	cmd.Flags().BoolVar(&validate, "validate", true, "run a Flux-compatible kustomize build of <path> and the Flux Kustomizations in it before committing, and refuse to push when it fails (for example when an overlay patches a service that is gone from Swarm)")
	cmd.Flags().DurationVar(&interval, "interval", 10*time.Minute, "with --watch: full resync period")
	cmd.Flags().DurationVar(&debounce, "debounce", 5*time.Second, "with --watch: wait this long after a Swarm change before exporting, so one stack deploy gives one commit")
	cmd.Flags().StringVar(&listen, "listen", ":9797", "with --watch: address for /healthz, /readyz, /status and /metrics; empty disables them")
	cmd.Flags().DurationVar(&unhealthyAfter, "unhealthy-after", 0, "with --watch: /healthz fails and flux4swarm_watch_healthy is 0 when no export succeeded for this long (default 3 x --interval)")
	cmd.Flags().StringArrayVar(&mergeEnvs, "merge", nil, "environment sharing a Kustomize base with --cluster (repeat for each, --cluster included, in the same order on every watcher; the first wins ties): rewrite --base-path and every listed clusters/ENV/apps after each export")
	cmd.Flags().StringVar(&basePath, "base-path", merge.DefaultBasePath, "with --merge: directory of the shared base in the repository")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		if (output == "") == (gf.url == "") {
			return fmt.Errorf("give exactly one of --output or --git-url")
		}
		if exclude != "" {
			re, err := regexp.Compile(exclude)
			if err != nil {
				return fmt.Errorf("--exclude: %w", err)
			}
			o.Exclude = re
		}
		if len(mergeEnvs) > 0 {
			if o.Path != "" && strings.Trim(path.Clean(strings.TrimPrefix(o.Path, "./")), "/") != "clusters/"+o.Cluster {
				return fmt.Errorf("--merge needs the default --path clusters/%s", o.Cluster)
			}
			if err := checkMergeEnvs(mergeEnvs, o.Cluster); err != nil {
				return err
			}
		}
		o.GitURL, o.Branch, o.GitProvider = gf.url, gf.branch, gf.provider
		if gf.secretRef != "" {
			o.GitSecret = gf.secretRef
		}
		dc, err := dockerClient()
		if err != nil {
			return err
		}
		defer dc.Close()
		store := creds.New(credsDir)
		secretName := gf.apply(store)
		var secret map[string][]byte
		if secretName != "" {
			secret = store.Lookup(secretName, git.SecretKeys...)
		}
		// pushed is the commit of the last export that changed Git.
		var pushed string
		// export writes the Swarm to --output or Git. In watch mode files
		// outside the apps directory are only created, never overwritten.
		export := func(ctx context.Context) (*importer.Result, error) {
			res, err := importer.Export(ctx, dc, o)
			if err != nil {
				return nil, err
			}
			var notes []string
			write := func(root string) error {
				if len(mergeEnvs) == 0 {
					return res.WriteTo(root, watch)
				}
				n, err := writeMerged(root, res, watch, mergeEnvs, basePath)
				notes = n
				return err
			}
			printNotes := func() {
				for _, n := range notes {
					fmt.Println("note:", n)
				}
			}
			if output != "" {
				if err := write(output); err != nil {
					return nil, err
				}
				if validate {
					if err := importer.Validate(output, res.Root); err != nil {
						return nil, err
					}
				}
				fmt.Printf("wrote %d files to %s\n", len(res.Files), filepath.Join(output, res.Root))
				printNotes()
				return res, nil
			}
			verb := "Import"
			if watch {
				verb = "Sync"
			}
			msg := message
			var describe func([]git.FileChange) string
			if msg == "" {
				msg = verb + " Docker Swarm state into " + res.Root
				describe = func(changes []git.FileChange) string { return importer.CommitMessage(verb, changes) }
			}
			work, err := os.MkdirTemp("", "flux4swarm-import-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(work)
			pr, err := git.CommitAndPush(ctx, filepath.Join(work, "repo"), git.PushOptions{
				URL: gf.url, Branch: gf.branch, Message: msg, Describe: describe, Author: author, Email: email,
				Auth: &git.AuthProvider{Provider: gf.provider, URL: gf.url, Secret: secret},
				Write: func(root string) error {
					if err := write(root); err != nil {
						return err
					}
					if validate {
						return importer.Validate(root, res.Root)
					}
					return nil
				},
			})
			if err != nil {
				return nil, err
			}
			printNotes()
			if pr.Changed {
				pushed = pr.Commit
				fmt.Printf("pushed %d files to %s (%s) commit %s\n", len(res.Files), gf.url, gf.branch, pr.Commit)
			} else {
				fmt.Printf("no changes; %s (%s) is up to date\n", gf.url, gf.branch)
			}
			return res, nil
		}
		if watch {
			if o.Adopt {
				return fmt.Errorf("--adopt hands the Swarm over to the controller; it cannot be combined with --watch, where Swarm stays the source of truth")
			}
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			if unhealthyAfter <= 0 {
				unhealthyAfter = 3 * interval
			}
			status := importer.NewWatchStatus(unhealthyAfter)
			if listen != "" {
				srv := &http.Server{Addr: listen, Handler: status.Handler(), ReadHeaderTimeout: 10 * time.Second}
				go func() {
					if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
						slog.Error("status server", "error", err)
					}
				}()
				defer func() {
					shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdown)
				}()
			}
			var last string
			slog.Info("watching Swarm", "path", o.Path, "interval", interval, "debounce", debounce, "listen", listen, "unhealthy-after", unhealthyAfter)
			return importer.Watch(ctx, dc, importer.WatchOptions{Interval: interval, Debounce: debounce, Log: slog.Default(), Status: status},
				func(ctx context.Context, full bool) error {
					// Cheap check first: the export is deterministic, so an
					// unchanged hash means Git is already up to date, unless
					// someone edited it (the periodic full resync repairs that).
					if !full {
						res, err := importer.Export(ctx, dc, o)
						if err != nil {
							return err
						}
						if res.Hash() == last {
							slog.Debug("export unchanged")
							return nil
						}
					}
					pushed = ""
					res, err := export(ctx)
					if err != nil {
						return err
					}
					if pushed != "" {
						status.Committed(pushed)
					}
					last = res.Hash()
					for _, sk := range res.Skipped {
						slog.Debug("skipped", "object", sk.String())
					}
					return nil
				})
		}
		res, err := export(ctx)
		if err != nil {
			return err
		}
		if o.Adopt && res.Desired != nil {
			// Label the services the export reproduces exactly as owned by
			// the flux-system Kustomization, so the first reconcile is a
			// no-op instead of an update that only adds labels.
			a := &swarm.Applier{Client: dc, Owner: swarm.Owner{Name: adoptOwner, Namespace: api.DefaultNamespace}}
			changes, err := a.AdoptLabels(ctx, *res.Desired)
			for _, ch := range changes {
				fmt.Printf("%s/%s %s (%s)\n", ch.Kind, ch.Name, ch.Action, ch.Reason)
			}
			if err != nil {
				return err
			}
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
