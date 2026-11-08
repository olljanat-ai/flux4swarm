// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/olljanat-ai/flux4swarm/internal/importer"
	"github.com/olljanat-ai/flux4swarm/internal/volumebackup"
)

func newVolumeBackupCmd() *cobra.Command {
	var (
		o    volumebackup.Options
		env  []string
		stop bool
	)
	cmd := &cobra.Command{
		Use:   "volume-backup VOLUME...",
		Short: "Copy Swarm volumes to restic repositories that VolSync restores on Kubernetes",
		Long: "Back up each named Swarm volume (as shown by docker volume ls, e.g. shop_db) to\n" +
			"<repository>/<namespace>/<claim>, the restic repository that the VolSync\n" +
			"ReplicationDestination written by `import --volsync` restores into the\n" +
			"PersistentVolumeClaim of the same name.\n\n" +
			"A one-off replicated-job service mounts the volume exactly like the service\n" +
			"that uses it (same driver and options, so any volume plugin works) read-only\n" +
			"at /data, on the node that runs that service, and runs restic there. The job\n" +
			"and its secret are removed afterwards. Backups are incremental: run it while\n" +
			"the service runs to pre-seed, then stop the service and run it again for a\n" +
			"consistent final copy.\n\n" +
			"With --every the copy runs continuously instead: a long-running sync service\n" +
			"backs up every interval (pair it with import --volsync-schedule so Kubernetes\n" +
			"restores on a schedule too). At cutover stop the service on Swarm and run\n" +
			"volume-backup once more without --every: it removes the sync service and takes\n" +
			"the final increment.\n\n" +
			"The repository settings come from the environment: RESTIC_REPOSITORY (or\n" +
			"--repository), RESTIC_PASSWORD and the --env variables (S3 credentials by\n" +
			"default). Use the same values the secret store holds for Kubernetes.",
		Args: cobra.MinimumNArgs(1),
	}
	cmd.Flags().StringVar(&o.Repository, "repository", os.Getenv("RESTIC_REPOSITORY"), "base restic repository, e.g. s3:https://s3.example.com/bucket/swarm-prod (env RESTIC_REPOSITORY)")
	cmd.Flags().StringSliceVar(&env, "env", importer.DefaultVolSyncEnv, "environment variables passed to restic besides RESTIC_PASSWORD (same list as import --volsync-env)")
	cmd.Flags().StringVar(&o.Image, "image", volumebackup.DefaultImage, "restic image of the backup job")
	cmd.Flags().StringVar(&o.Node, "node", "", "run the job on this node (ID or hostname) instead of the node that runs the service")
	cmd.Flags().StringSliceVar(&o.Networks, "network", nil, "attach the backup job to these networks, e.g. to reach a repository server running on the Swarm")
	cmd.Flags().DurationVar(&o.Timeout, "timeout", time.Hour, "how long to wait for each backup")
	cmd.Flags().DurationVar(&o.Every, "every", 0, "instead of one backup, start a long-running sync service that backs up incrementally at this interval (e.g. 15m) until it is stopped; a later one-off volume-backup of the volume (the final copy) or --stop removes it")
	cmd.Flags().IntVar(&o.Keep, "keep", 24, "with --every: snapshots to keep; every --keep rounds the older ones are forgotten and pruned")
	cmd.Flags().BoolVar(&o.Restore, "restore", false, "the other way round, to roll back: restore the newest snapshot of each volume's repository (for example one VolSync wrote from the claim on Kubernetes) into the Swarm volume, deleting files that are not in it; the services using the volume must be stopped")
	cmd.Flags().BoolVar(&stop, "stop", false, "remove the sync services of the volumes and do nothing else")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		o.Env = map[string]string{"RESTIC_PASSWORD": os.Getenv("RESTIC_PASSWORD")}
		for _, k := range env {
			if k = strings.TrimSpace(k); k == "" {
				continue
			}
			if v, ok := os.LookupEnv(k); ok {
				o.Env[k] = v
			}
		}
		o.Logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
		// Stop waiting on Ctrl-C or SIGTERM, but still remove the job.
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		dc, err := dockerClient()
		if err != nil {
			return err
		}
		defer dc.Close()
		for _, v := range args {
			if stop {
				names, err := volumebackup.StopSync(ctx, dc, v, 0)
				if err != nil {
					return err
				}
				if len(names) == 0 {
					fmt.Fprintf(os.Stderr, "volume %s has no sync service\n", v)
				}
				for _, n := range names {
					fmt.Printf("%s\tstopped %s\n", v, n)
				}
				continue
			}
			r, err := volumebackup.Backup(ctx, dc, v, o)
			if err != nil {
				return err
			}
			fmt.Printf("%s\t%s/%s\t%s\n", r.Volume, r.Namespace, r.Claim, r.Repository)
		}
		return nil
	}
	return cmd
}
