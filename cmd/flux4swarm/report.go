// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/olljanat-ai/flux4swarm/internal/importer"
)

func newReportCmd() *cobra.Command {
	var (
		o       importer.Options
		exclude string
		asJSON  bool
		output  string
		fail    bool
	)
	cmd := &cobra.Command{
		Use:   "migration-report",
		Short: "Report what blocks moving each Swarm service to Kubernetes",
		Long: "Read the Swarm like import does and print the Kubernetes migration readiness\n" +
			"report: per service, the blockers (bind mounts, the Docker socket, host mode\n" +
			"ports, placement constraints without a nodeSelector, plain text credentials in\n" +
			"the environment) and the checks (settings Kubernetes ignores, load balancers,\n" +
			"node labels, storage classes). import writes the same report to\n" +
			"<path>/" + importer.ReportFile + " on every export. Nothing is changed.",
		Args: cobra.NoArgs,
	}
	cmd.Flags().StringVar(&o.Cluster, "cluster", "default", "cluster name, as for import")
	cmd.Flags().StringVar(&o.Path, "path", "", "cluster directory, as for import (default clusters/<cluster>)")
	cmd.Flags().StringVar(&exclude, "exclude", "", "regular expression of Swarm object names to skip")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of Markdown")
	cmd.Flags().StringVar(&output, "output", "", "write the report to this file instead of stdout")
	cmd.Flags().BoolVar(&fail, "fail-on-blockers", false, "exit with status 2 when any service has a blocker")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if exclude != "" {
			re, err := regexp.Compile(exclude)
			if err != nil {
				return fmt.Errorf("--exclude: %w", err)
			}
			o.Exclude = re
		}
		dc, err := dockerClient()
		if err != nil {
			return err
		}
		defer dc.Close()
		res, err := importer.Export(cmd.Context(), dc, o)
		if err != nil {
			return err
		}
		out := res.Files[res.Root+"/"+importer.ReportFile]
		if asJSON {
			if out, err = json.MarshalIndent(res.Report, "", "  "); err != nil {
				return err
			}
			out = append(out, '\n')
		}
		if output != "" {
			err = os.WriteFile(output, out, 0o644)
		} else {
			_, err = os.Stdout.Write(out)
		}
		if err != nil {
			return err
		}
		if fail {
			for _, s := range res.Report.Services {
				if s.Blockers() > 0 {
					dc.Close()
					os.Exit(2)
				}
			}
		}
		return nil
	}
	return cmd
}
