// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/build"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	"github.com/olljanat-ai/flux4swarm/internal/git"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
	"github.com/olljanat-ai/flux4swarm/internal/merge"
)

func newMergeCmd() *cobra.Command {
	var (
		gf       gitFlags
		envFlags []string
		basePath string
		output   string
		message  string
		author   string
		email    string
		credsDir string
	)
	cmd := &cobra.Command{
		Use:   "merge",
		Short: "Combine the imports of several Swarms (environments) into a shared Kustomize base and per-environment overlays",
		Long: `merge reads clusters/ENV/apps of every --env (written by "flux4swarm import --cluster ENV",
or by an earlier merge), moves what the environments share to --base-path and rewrites each
clusters/ENV/apps as a kustomize overlay: the base, the objects only that environment has and
standard patches for what differs (image tags, replicas, env vars, config data, hosts...).
Each overlay builds back to exactly its environment's import. Only --base-path and
clusters/ENV/apps are rewritten; flux-system and hand-written files stay as they are.

--env NAME=DIR reads that environment from a separate local import instead (DIR/clusters/NAME/apps,
DIR/apps or DIR) and writes its overlay to clusters/NAME/apps of the target.`,
		Example: `  DOCKER_HOST=ssh://dev-manager  flux4swarm import --cluster development --bootstrap --output repo
  DOCKER_HOST=ssh://acc-manager  flux4swarm import --cluster acceptance  --bootstrap --output repo
  DOCKER_HOST=ssh://prod-manager flux4swarm import --cluster production  --bootstrap --output repo
  flux4swarm merge --env development --env acceptance --env production --output repo`,
		Args: cobra.NoArgs,
	}
	gf.register(cmd, "git-url")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "environment NAME or NAME=DIR (repeat, at least two; the first wins ties)")
	cmd.Flags().StringVar(&basePath, "base-path", merge.DefaultBasePath, "directory of the shared base in the repository")
	cmd.Flags().StringVar(&output, "output", "", "local repository directory to merge in place, instead of Git")
	cmd.Flags().StringVar(&message, "commit-message", "", "commit message")
	cmd.Flags().StringVar(&author, "author", "flux4swarm", "commit author name")
	cmd.Flags().StringVar(&email, "author-email", "flux4swarm@users.noreply.github.com", "commit author email")
	cmd.Flags().StringVar(&credsDir, "credentials-dir", creds.DefaultDir, "directory with the --git-secret credentials")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if (output == "") == (gf.url == "") {
			return fmt.Errorf("give exactly one of --output or --git-url")
		}
		specs, err := parseEnvs(envFlags)
		if err != nil {
			return err
		}
		var res *importer.Result
		run := func(root string) (err error) {
			res, err = mergeInto(root, specs, basePath)
			return err
		}
		if output != "" {
			if err := run(output); err != nil {
				return err
			}
			fmt.Printf("wrote %d files to %s\n", len(res.Files), output)
		} else {
			store := creds.New(credsDir)
			secretName := gf.apply(store)
			var secret map[string][]byte
			if secretName != "" {
				secret = store.Lookup(secretName, git.SecretKeys...)
			}
			if message == "" {
				var names []string
				for _, s := range specs {
					names = append(names, s.name)
				}
				message = "Merge " + strings.Join(names, ", ") + " into a shared base"
			}
			work, err := os.MkdirTemp("", "flux4swarm-merge-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(work)
			pr, err := git.CommitAndPush(cmd.Context(), filepath.Join(work, "repo"), git.PushOptions{
				URL: gf.url, Branch: gf.branch, Message: message, Author: author, Email: email,
				Auth:  &git.AuthProvider{Provider: gf.provider, URL: gf.url, Secret: secret},
				Write: run,
			})
			if err != nil {
				return err
			}
			if pr.Changed {
				fmt.Printf("pushed %d files to %s (%s) commit %s\n", len(res.Files), gf.url, gf.branch, pr.Commit)
			} else {
				fmt.Printf("no changes; %s (%s) is up to date\n", gf.url, gf.branch)
			}
		}
		for _, n := range res.Notes {
			fmt.Println("note:", n)
		}
		return nil
	}
	return cmd
}

type envSpec struct{ name, dir string }

func parseEnvs(flags []string) ([]envSpec, error) {
	if len(flags) < 2 {
		return nil, fmt.Errorf("give at least two --env")
	}
	var out []envSpec
	for _, f := range flags {
		name, dir, _ := strings.Cut(f, "=")
		if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return nil, fmt.Errorf("--env %q: invalid environment name", f)
		}
		out = append(out, envSpec{name: name, dir: dir})
	}
	return out, nil
}

// loadEnvs builds every environment's apps directory.
func loadEnvs(root string, specs []envSpec) ([]merge.Env, error) {
	var envs []merge.Env
	for _, s := range specs {
		appsPath := "clusters/" + s.name + "/" + importer.AppsDir
		srcRoot, srcPath := root, appsPath
		if s.dir != "" {
			srcRoot, srcPath = s.dir, "."
			for _, p := range []string{appsPath, importer.AppsDir} {
				if st, err := os.Stat(filepath.Join(s.dir, filepath.FromSlash(p))); err == nil && st.IsDir() {
					srcPath = p
					break
				}
			}
		}
		out, err := build.Render(srcRoot, build.Options{Spec: api.KustomizationSpec{Path: srcPath}})
		if err != nil {
			return nil, fmt.Errorf("environment %s (%s): %w", s.name, filepath.Join(srcRoot, srcPath), err)
		}
		objs, err := merge.Load(out)
		if err != nil {
			return nil, fmt.Errorf("environment %s: %w", s.name, err)
		}
		envs = append(envs, merge.Env{Name: s.name, Path: appsPath, Objects: objs})
	}
	return envs, nil
}

// mergeInto rebuilds the shared base and the overlays of specs in root.
func mergeInto(root string, specs []envSpec, basePath string) (*importer.Result, error) {
	envs, err := loadEnvs(root, specs)
	if err != nil {
		return nil, err
	}
	res, err := merge.Merge(envs, merge.Options{BasePath: basePath})
	if err != nil {
		return nil, err
	}
	return res, res.WriteTo(root, false)
}

// checkMergeEnvs checks import --merge: the environments sharing the base,
// in merge order, one of them cluster.
func checkMergeEnvs(names []string, cluster string) error {
	specs, err := parseEnvs(names)
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), "--env", "--merge"))
	}
	found := false
	for _, s := range specs {
		if s.dir != "" {
			return fmt.Errorf("--merge %s=%s: give environment names only", s.name, s.dir)
		}
		found = found || s.name == cluster
	}
	if !found {
		return fmt.Errorf("--merge must list --cluster %s", cluster)
	}
	return nil
}

// writeMerged writes one environment's export to root and then regenerates
// the shared base and the overlays of every environment in envs (merge
// order) that has clusters/ENV/apps. With fewer than two, the export is
// left as written.
func writeMerged(root string, res *importer.Result, keep bool, envs []string, basePath string) ([]string, error) {
	if err := res.WriteTo(root, keep); err != nil {
		return nil, err
	}
	var specs []envSpec
	var notes []string
	for _, n := range envs {
		if st, err := os.Stat(filepath.Join(root, "clusters", n, importer.AppsDir)); err != nil || !st.IsDir() {
			notes = append(notes, fmt.Sprintf("environment %s has no export yet; left out of the shared base", n))
			continue
		}
		specs = append(specs, envSpec{name: n})
	}
	if len(specs) < 2 {
		return notes, nil
	}
	m, err := mergeInto(root, specs, basePath)
	if err != nil {
		return nil, err
	}
	return append(notes, m.Notes...), nil
}
