// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package kube talks to a Kubernetes API server for kube-import and
// kube-backup, with the credentials of a kubeconfig (KUBECONFIG,
// ~/.kube/config or the in-cluster service account).
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/olljanat-ai/flux4swarm/internal/importer"
)

// FieldManager is the server-side apply field manager of flux4swarm.
const FieldManager = "flux4swarm"

// Client is a discovery and dynamic client for one cluster.
type Client struct {
	http *http.Client
	dyn  dynamic.Interface
	// Host is the API server URL.
	Host string
}

// New connects with the kubeconfig at path (default: the usual loading
// rules) and its context (default: current-context).
func New(path, kubeContext string) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cfg.QPS, cfg.Burst = 50, 100
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{http: hc, dyn: dyn, Host: strings.TrimSuffix(cfg.Host, "/")}, nil
}

// Resources returns the preferred version of every kind that can be
// listed. API groups that fail discovery (an unavailable aggregated API,
// such as a broken metrics-server) are left out.
func (c *Client) Resources(ctx context.Context) ([]importer.KubeResource, error) {
	var core metav1.APIResourceList
	if err := c.getJSON(ctx, "/api/v1", &core); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	lists := []*metav1.APIResourceList{&core}
	var groups metav1.APIGroupList
	if err := c.getJSON(ctx, "/apis", &groups); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	for _, g := range groups.Groups {
		gv := g.PreferredVersion.GroupVersion
		if gv == "" && len(g.Versions) > 0 {
			gv = g.Versions[0].GroupVersion
		}
		var l metav1.APIResourceList
		if err := c.getJSON(ctx, "/apis/"+gv, &l); err != nil {
			slog.Warn("discovery: API group left out", "groupVersion", gv, "error", err)
			continue
		}
		if l.GroupVersion == "" {
			l.GroupVersion = gv
		}
		lists = append(lists, &l)
	}
	var out []importer.KubeResource
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") || !contains(r.Verbs, "list") {
				continue
			}
			out = append(out, importer.KubeResource{Group: gv.Group, Version: gv.Version, Resource: r.Name, Kind: r.Kind, Namespaced: r.Namespaced})
		}
	}
	return out, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Host+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) ri(r importer.KubeResource, ns string) dynamic.ResourceInterface {
	nri := c.dyn.Resource(schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Resource})
	if r.Namespaced && ns != "" {
		return nri.Namespace(ns)
	}
	return nri
}

// List returns every object of r in ns ("" for all), page by page.
func (c *Client) List(ctx context.Context, r importer.KubeResource, ns string) ([]map[string]any, error) {
	var out []map[string]any
	opts := metav1.ListOptions{Limit: 500}
	for {
		l, err := c.ri(r, ns).List(ctx, opts)
		if err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err) {
				// Not allowed to read it, or gone since discovery.
				return out, nil
			}
			return nil, err
		}
		for i := range l.Items {
			out = append(out, l.Items[i].Object)
		}
		if l.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = l.GetContinue()
	}
}

// Get returns one object, or nil when it does not exist.
func (c *Client) Get(ctx context.Context, r importer.KubeResource, ns, name string) (map[string]any, error) {
	u, err := c.ri(r, ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u.Object, nil
}

// Apply server-side applies obj (apiVersion, kind and metadata set), taking
// over conflicting fields.
func (c *Client) Apply(ctx context.Context, r importer.KubeResource, obj map[string]any) error {
	u := &unstructured.Unstructured{Object: obj}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	force := true
	_, err = c.ri(r, u.GetNamespace()).Patch(ctx, u.GetName(), types.ApplyPatchType, b,
		metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

// MergePatch applies a JSON merge patch to one object.
func (c *Client) MergePatch(ctx context.Context, r importer.KubeResource, ns, name string, patch map[string]any) error {
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.ri(r, ns).Patch(ctx, name, types.MergePatchType, b, metav1.PatchOptions{FieldManager: FieldManager})
	return err
}

// Delete deletes one object; a missing object is not an error.
func (c *Client) Delete(ctx context.Context, r importer.KubeResource, ns, name string) error {
	err := c.ri(r, ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
