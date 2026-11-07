// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/controller"
)

// apiClient talks to a running controller's status API, like the flux CLI
// talks to the Kubernetes API.
type apiClient struct {
	server    string
	token     string
	namespace string
	all       bool
	http      *http.Client
}

func defaultServer() string {
	if v := os.Getenv("FLUX4SWARM_SERVER"); v != "" {
		return v
	}
	return "http://127.0.0.1:9797"
}

func (c *apiClient) register(cmd *cobra.Command, allNamespaces bool) {
	cmd.PersistentFlags().StringVar(&c.server, "server", defaultServer(), "URL of the controller's status API (env FLUX4SWARM_SERVER)")
	cmd.PersistentFlags().StringVar(&c.token, "api-token", "", "bearer token for the API (env FLUX4SWARM_API_TOKEN)")
	cmd.PersistentFlags().StringVarP(&c.namespace, "namespace", "n", api.DefaultNamespace, "namespace of the object")
	if allNamespaces {
		cmd.PersistentFlags().BoolVarP(&c.all, "all-namespaces", "A", false, "list objects in all namespaces")
	}
}

func (c *apiClient) do(ctx context.Context, method, path string) ([]byte, error) {
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.server, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	token := c.token
	if token == "" {
		token = os.Getenv("FLUX4SWARM_API_TOKEN")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (c *apiClient) status(ctx context.Context) ([]controller.ObjectStatus, error) {
	b, err := c.do(ctx, http.MethodGet, "/status")
	if err != nil {
		return nil, err
	}
	var st []controller.ObjectStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("decode /status: %w", err)
	}
	return st, nil
}

// ---- get ----

func newGetCmd() *cobra.Command {
	c := &apiClient{}
	var output string
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Show the status of the objects of a running controller (like flux get)",
	}
	c.register(cmd, true)
	cmd.PersistentFlags().StringVarP(&output, "output", "o", "table", "table or json")
	sub := func(use string, aliases []string, short string, kinds ...string) *cobra.Command {
		return &cobra.Command{
			Use:     use + " [name]",
			Aliases: aliases,
			Short:   short,
			Args:    cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				st, err := c.status(cmd.Context())
				if err != nil {
					return err
				}
				name := ""
				if len(args) == 1 {
					name = args[0]
				}
				sel := c.filter(st, name, kinds...)
				if name != "" && len(sel) == 0 {
					return fmt.Errorf("%s %s/%s not found", strings.Join(kinds, " or "), c.namespace, name)
				}
				return printObjects(cmd.OutOrStdout(), output, sel, len(kinds) > 1, c.all)
			},
		}
	}
	sources := &cobra.Command{Use: "sources", Aliases: []string{"source"}, Short: "Show sources"}
	sources.AddCommand(sub("git", nil, "Show GitRepository sources", api.KindGitRepository, controller.LocalSourceKind))
	sources.AddCommand(sub("oci", nil, "Show OCIRepository sources", api.KindOCIRepository))
	sources.AddCommand(sub("all", nil, "Show all sources", api.KindGitRepository, api.KindOCIRepository, controller.LocalSourceKind))
	cmd.AddCommand(
		sub("kustomizations", []string{"kustomization", "ks"}, "Show Kustomizations", api.KindKustomization),
		sources,
		sub("all", nil, "Show all objects", api.KindGitRepository, api.KindOCIRepository, controller.LocalSourceKind, api.KindKustomization),
	)
	return cmd
}

func (c *apiClient) filter(st []controller.ObjectStatus, name string, kinds ...string) []controller.ObjectStatus {
	var out []controller.ObjectStatus
	for _, s := range st {
		kindOK := false
		for _, k := range kinds {
			kindOK = kindOK || s.Kind == k
		}
		if !kindOK || (!c.all && s.Namespace != c.namespace) || (name != "" && s.Name != name) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func printObjects(w io.Writer, output string, st []controller.ObjectStatus, withKind, withNS bool) error {
	switch output {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if st == nil {
			st = []controller.ObjectStatus{}
		}
		return enc.Encode(st)
	case "table", "":
	default:
		return fmt.Errorf("--output must be table or json")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	header := "NAME\tREVISION\tSUSPENDED\tREADY\tSKIPPED\tMESSAGE"
	if withNS {
		header = "NAMESPACE\t" + header
	}
	fmt.Fprintln(tw, header)
	for _, s := range st {
		name := s.Name
		if withKind {
			name = strings.ToLower(s.Kind) + "/" + name
		}
		rev := s.Revision
		if s.Kind == api.KindKustomization {
			rev = s.LastAppliedRevision
		}
		ready := s.Ready.Status
		if ready == "" {
			ready = controller.ConditionUnknown
		}
		skipped := "-"
		if s.Kind == api.KindKustomization {
			skipped = fmt.Sprint(len(s.Skipped))
		}
		row := fmt.Sprintf("%s\t%s\t%t\t%s\t%s\t%s", name, rev, s.Suspended, ready, skipped, oneLine(s.Ready.Message))
		if withNS {
			row = s.Namespace + "\t" + row
		}
		fmt.Fprintln(tw, row)
	}
	return tw.Flush()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ---- suspend / resume ----

func newSuspendCmd(suspend bool) *cobra.Command {
	c := &apiClient{}
	var (
		wait    bool
		timeout time.Duration
	)
	action, short := "suspend", "Suspend reconciliation of an object of a running controller (like flux suspend)"
	if !suspend {
		action, short = "resume", "Resume a suspended object of a running controller and wait for it to reconcile (like flux resume)"
	}
	cmd := &cobra.Command{Use: action, Short: short}
	c.register(cmd, false)
	if !suspend {
		cmd.PersistentFlags().BoolVar(&wait, "wait", true, "wait for the object to reconcile")
		cmd.PersistentFlags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait")
	}
	run := func(kind string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Remember when each object last reconciled, to wait for the
			// next one. The server's clock is used, not ours.
			before := map[string]time.Time{}
			if !suspend && wait {
				st, err := c.status(ctx)
				if err != nil {
					return err
				}
				for _, s := range st {
					if s.Kind == kind && s.Namespace == c.namespace {
						before[s.Name] = s.LastReconcile
					}
				}
			}
			for _, name := range args {
				p := fmt.Sprintf("/%s/%s/%s/%s", action, url.PathEscape(strings.ToLower(kind)),
					url.PathEscape(c.namespace), url.PathEscape(name))
				b, err := c.do(ctx, http.MethodPost, p)
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), string(b))
			}
			if suspend || !wait {
				return nil
			}
			return c.waitReconciled(ctx, cmd.OutOrStdout(), kind, args, before, timeout)
		}
	}
	ks := &cobra.Command{
		Use: "kustomization NAME...", Aliases: []string{"kustomizations", "ks"},
		Short: action + " Kustomizations", Args: cobra.MinimumNArgs(1), RunE: run(api.KindKustomization),
	}
	source := &cobra.Command{Use: "source", Short: action + " sources"}
	source.AddCommand(&cobra.Command{
		Use: "git NAME...", Short: action + " GitRepositories", Args: cobra.MinimumNArgs(1), RunE: run(api.KindGitRepository),
	}, &cobra.Command{
		Use: "oci NAME...", Short: action + " OCIRepositories", Args: cobra.MinimumNArgs(1), RunE: run(api.KindOCIRepository),
	})
	cmd.AddCommand(ks, source)
	return cmd
}

// waitReconciled polls /status until every named object reconciled again
// (its lastReconcile moved past before), and fails if one is not Ready.
func (c *apiClient) waitReconciled(ctx context.Context, w io.Writer, kind string, names []string,
	before map[string]time.Time, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pending := map[string]bool{}
	for _, n := range names {
		pending[n] = true
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		st, err := c.status(ctx)
		if err != nil {
			return err
		}
		for _, s := range st {
			if s.Kind != kind || s.Namespace != c.namespace || !pending[s.Name] || !s.LastReconcile.After(before[s.Name]) {
				continue
			}
			if s.Ready.Status == controller.ConditionUnknown && s.Ready.Reason == controller.ReasonProgressing {
				continue
			}
			delete(pending, s.Name)
			if !s.IsReady() {
				return fmt.Errorf("%s %s/%s: %s", kind, s.Namespace, s.Name, s.Ready.Message)
			}
			fmt.Fprintf(w, "%s %s/%s reconciled: %s\n", kind, s.Namespace, s.Name, s.Ready.Message)
		}
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s to reconcile", strings.Join(sortedKeys(pending), ", "))
		case <-tick.C:
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
