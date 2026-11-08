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
		o   volumebackup.Options
		env []string
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
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		dc, err := dockerClient()
		if err != nil {
			return err
		}
		defer dc.Close()
		for _, v := range args {
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
