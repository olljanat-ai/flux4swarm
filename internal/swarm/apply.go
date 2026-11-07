// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package swarm applies desired state to Docker Swarm: networks, configs,
// secrets and services, with ownership labels, pruning and convergence
// waits.
package swarm

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/swarm/tfdocker"
)

// Client is the Docker API subset the applier uses. *client.Client
// implements it.
type Client interface {
	tfdocker.Client
	Info(ctx context.Context) (system.Info, error)
	ServiceList(ctx context.Context, options swarm.ServiceListOptions) ([]swarm.Service, error)
	ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error)
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
	ServiceRemove(ctx context.Context, serviceID string) error
	ConfigList(ctx context.Context, options swarm.ConfigListOptions) ([]swarm.Config, error)
	ConfigCreate(ctx context.Context, config swarm.ConfigSpec) (swarm.ConfigCreateResponse, error)
	ConfigRemove(ctx context.Context, id string) error
	SecretList(ctx context.Context, options swarm.SecretListOptions) ([]swarm.Secret, error)
	SecretCreate(ctx context.Context, secret swarm.SecretSpec) (swarm.SecretCreateResponse, error)
	SecretRemove(ctx context.Context, id string) error
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkRemove(ctx context.Context, networkID string) error
}

// Owner identifies the Kustomization that owns objects.
type Owner struct {
	Name      string
	Namespace string
}

// Labels returns the ownership labels.
func (o Owner) Labels() map[string]string {
	return map[string]string{
		api.LabelKustomizeName:      o.Name,
		api.LabelKustomizeNamespace: o.Namespace,
		api.LabelManagedBy:          api.ManagedByValue,
	}
}

func (o Owner) filter() filters.Args {
	return filters.NewArgs(
		filters.Arg("label", api.LabelKustomizeName+"="+o.Name),
		filters.Arg("label", api.LabelKustomizeNamespace+"="+o.Namespace),
	)
}

func (o Owner) owns(labels map[string]string) bool {
	return labels[api.LabelKustomizeName] == o.Name && labels[api.LabelKustomizeNamespace] == o.Namespace
}

// Secret is a resolved secret ready to be created in Swarm.
type Secret struct {
	Name   string
	Data   []byte
	Labels map[string]string
	// Source is the ExternalSecret it came from ("namespace/name").
	Source string
}

// Kinds used in inventories and change lists.
const (
	KindService = "Service"
	KindConfig  = "Config"
	KindSecret  = "Secret"
	KindNetwork = "Network"
)

// Network is a desired overlay network.
type Network struct {
	Name   string
	Labels map[string]string
	Spec   network.CreateOptions
}

// Config is a desired Docker config; Name is the logical name.
type Config struct {
	Name       string
	Data       []byte
	Labels     map[string]string
	Templating string
}

// Service is a desired Swarm service. Config and secret references use
// logical names.
type Service struct {
	Name string
	Spec swarm.ServiceSpec
	// RegistryAuth overrides the applier's registry credentials (from
	// imagePullSecrets).
	RegistryAuth map[string]registry.AuthConfig
}

// Desired is the state one Kustomization wants.
type Desired struct {
	Networks []*Network
	Configs  []*Config
	Secrets  []Secret
	Services []*Service
}

// Action is what happened to an object.
type Action string

const (
	Created    Action = "created"
	Configured Action = "configured"
	Unchanged  Action = "unchanged"
	Deleted    Action = "deleted"
	Skipped    Action = "skipped"
)

// Change records one action.
type Change struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action Action `json:"action"`
	Reason string `json:"reason,omitempty"`
}

func (c Change) String() string {
	s := fmt.Sprintf("%s/%s %s", c.Kind, c.Name, c.Action)
	if c.Reason != "" {
		s += " (" + c.Reason + ")"
	}
	return s
}

// Result of Apply.
type Result struct {
	Changes []Change
	// Inventory lists "Kind/name" of every object in the desired state.
	Inventory []string
	// Versions lists the Swarm names of the config and secret versions the
	// desired services use. Prune keeps them.
	Versions []string
	// ServiceIDs of the desired services, by name.
	ServiceIDs map[string]string
}

func (r *Result) add(kind, name string, a Action, reason string) {
	r.Changes = append(r.Changes, Change{Kind: kind, Name: name, Action: a, Reason: reason})
}

// Applier applies Desired state for one owner.
type Applier struct {
	Client       Client
	Owner        Owner
	RegistryAuth map[string]registry.AuthConfig
	DryRun       bool
	// Force recreates services whose update fails (like Flux's spec.force).
	Force bool
	Log   *slog.Logger

	hashKey []byte
}

type versioned struct {
	id   string
	name string
}

func (a *Applier) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// key used to derive secret version names. It is the Swarm cluster ID, so
// the same secret value gives different names in different clusters and
// names cannot be looked up in a precomputed table.
func (a *Applier) key(ctx context.Context) ([]byte, error) {
	if a.hashKey != nil {
		return a.hashKey, nil
	}
	info, err := a.Client.Info(ctx)
	if err != nil {
		return nil, err
	}
	if info.Swarm.LocalNodeState != swarm.LocalNodeStateActive || !info.Swarm.ControlAvailable {
		return nil, fmt.Errorf("docker engine is not a swarm manager (state %q)", info.Swarm.LocalNodeState)
	}
	a.hashKey = []byte(info.Swarm.Cluster.ID)
	return a.hashKey, nil
}

// VersionName returns "<name>-<10 hex chars>" for content-addressed configs.
func VersionName(name string, parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return name + "-" + hex.EncodeToString(h.Sum(nil))[:10]
}

func secretVersionName(key []byte, name string, data []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return name + "-" + hex.EncodeToString(m.Sum(nil))[:10]
}

// Apply creates or updates everything in d. It does not delete anything
// except older versions of configs and secrets that nothing uses any more.
func (a *Applier) Apply(ctx context.Context, d Desired) (*Result, error) {
	key, err := a.key(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{ServiceIDs: map[string]string{}}
	if err := checkUnique(d); err != nil {
		return nil, err
	}

	if err := a.applyNetworks(ctx, d.Networks, res); err != nil {
		return res, err
	}
	configs, err := a.applyConfigs(ctx, d.Configs, res)
	if err != nil {
		return res, err
	}
	secrets, err := a.applySecrets(ctx, key, d.Secrets, res)
	if err != nil {
		return res, err
	}
	for _, v := range configs {
		res.Versions = append(res.Versions, v.name)
	}
	for _, v := range secrets {
		res.Versions = append(res.Versions, v.name)
	}
	if err := a.applyServices(ctx, d.Services, configs, secrets, res); err != nil {
		return res, err
	}
	sort.Strings(res.Inventory)
	sort.Strings(res.Versions)
	return res, nil
}

func checkUnique(d Desired) error {
	seen := map[string]bool{}
	check := func(kind, name string) error {
		if name == "" {
			return fmt.Errorf("%s without a name", kind)
		}
		k := kind + "/" + name
		if seen[k] {
			return fmt.Errorf("duplicate %s", k)
		}
		seen[k] = true
		return nil
	}
	for _, n := range d.Networks {
		if err := check(KindNetwork, n.Name); err != nil {
			return err
		}
	}
	for _, c := range d.Configs {
		if err := check(KindConfig, c.Name); err != nil {
			return err
		}
	}
	for _, s := range d.Secrets {
		if err := check(KindSecret, s.Name); err != nil {
			return err
		}
	}
	for _, s := range d.Services {
		if err := check(KindService, s.Name); err != nil {
			return err
		}
	}
	return nil
}

// ---- networks ----

func (a *Applier) applyNetworks(ctx context.Context, desired []*Network, res *Result) error {
	if len(desired) == 0 {
		return nil
	}
	live, err := a.Client.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return err
	}
	byName := map[string]network.Summary{}
	for _, n := range live {
		byName[n.Name] = n
	}
	for _, n := range desired {
		name := n.Name
		res.Inventory = append(res.Inventory, KindNetwork+"/"+name)
		if cur, ok := byName[name]; ok {
			reason := ""
			if !a.Owner.owns(cur.Labels) {
				reason = "exists, not managed"
			}
			if want := n.Spec.Driver; want != "" && want != cur.Driver {
				reason = fmt.Sprintf("driver is %s, want %s; networks are immutable, delete it to recreate", cur.Driver, want)
			}
			res.add(KindNetwork, name, Unchanged, reason)
			continue
		}
		spec := n.Spec
		if spec.Driver == "" {
			spec.Driver = "overlay"
		}
		if spec.Driver == "overlay" && spec.Scope == "" {
			spec.Scope = "swarm"
		}
		spec.Labels = merge(spec.Labels, n.Labels, a.Owner.Labels())
		if !a.DryRun {
			if _, err := a.Client.NetworkCreate(ctx, name, spec); err != nil {
				return fmt.Errorf("create network %s: %w", name, err)
			}
		}
		res.add(KindNetwork, name, Created, "")
	}
	return nil
}

// ---- configs ----

func (a *Applier) applyConfigs(ctx context.Context, desired []*Config, res *Result) (map[string]versioned, error) {
	out := map[string]versioned{}
	if len(desired) == 0 {
		return out, nil
	}
	live, err := a.Client.ConfigList(ctx, swarm.ConfigListOptions{})
	if err != nil {
		return nil, err
	}
	byName := map[string]swarm.Config{}
	for _, c := range live {
		byName[c.Spec.Name] = c
	}
	for _, c := range desired {
		name := c.Name
		res.Inventory = append(res.Inventory, KindConfig+"/"+name)
		data := c.Data
		vname := VersionName(name, data, []byte(c.Templating))
		if cur, ok := byName[vname]; ok {
			out[name] = versioned{id: cur.ID, name: vname}
			res.add(KindConfig, name, Unchanged, "")
			continue
		}
		// Reuse an unmanaged config with identical content (preferring the
		// plain name), so taking over an imported Swarm does not roll
		// services just to swap identical configs.
		if cur, ok := adoptable(byName, live, name, data, c.Templating); ok {
			out[name] = versioned{id: cur.ID, name: cur.Spec.Name}
			res.add(KindConfig, name, Unchanged, "reusing existing config "+cur.Spec.Name)
			continue
		}
		spec := swarm.ConfigSpec{
			Annotations: swarm.Annotations{
				Name:   vname,
				Labels: merge(c.Labels, a.Owner.Labels(), map[string]string{api.LabelName: name}),
			},
			Data: data,
		}
		if c.Templating != "" {
			spec.Templating = &swarm.Driver{Name: c.Templating}
		}
		id := "dry-run"
		if !a.DryRun {
			r, err := a.Client.ConfigCreate(ctx, spec)
			if err != nil {
				return nil, fmt.Errorf("create config %s: %w", vname, err)
			}
			id = r.ID
		}
		out[name] = versioned{id: id, name: vname}
		action := Created
		for _, l := range live {
			if l.Spec.Labels[api.LabelName] == name && a.Owner.owns(l.Spec.Labels) {
				action = Configured
			}
		}
		res.add(KindConfig, name, action, "")
	}
	return out, nil
}

func adoptable(byName map[string]swarm.Config, live []swarm.Config, name string, data []byte, templating string) (swarm.Config, bool) {
	same := func(c swarm.Config) bool {
		return c.Spec.Labels[api.LabelManagedBy] != api.ManagedByValue &&
			string(c.Spec.Data) == string(data) && templatingName(c.Spec.Templating) == templating
	}
	if cur, ok := byName[name]; ok && same(cur) {
		return cur, true
	}
	var found []swarm.Config
	for _, c := range live {
		if same(c) {
			found = append(found, c)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return swarm.Config{}, false
}

func templatingName(d *swarm.Driver) string {
	if d == nil {
		return ""
	}
	return d.Name
}

// ---- secrets ----

func (a *Applier) applySecrets(ctx context.Context, key []byte, desired []Secret, res *Result) (map[string]versioned, error) {
	out := map[string]versioned{}
	if len(desired) == 0 {
		return out, nil
	}
	live, err := a.Client.SecretList(ctx, swarm.SecretListOptions{})
	if err != nil {
		return nil, err
	}
	byName := map[string]swarm.Secret{}
	for _, s := range live {
		byName[s.Spec.Name] = s
	}
	for _, s := range desired {
		res.Inventory = append(res.Inventory, KindSecret+"/"+s.Name)
		vname := secretVersionName(key, s.Name, s.Data)
		if cur, ok := byName[vname]; ok {
			out[s.Name] = versioned{id: cur.ID, name: vname}
			res.add(KindSecret, s.Name, Unchanged, "")
			continue
		}
		labels := merge(s.Labels, a.Owner.Labels(), map[string]string{api.LabelName: s.Name})
		if s.Source != "" {
			labels[api.LabelExternalSecret] = strings.ReplaceAll(s.Source, "/", ".")
		}
		spec := swarm.SecretSpec{
			Annotations: swarm.Annotations{Name: vname, Labels: labels},
			Data:        s.Data,
		}
		id := "dry-run"
		if !a.DryRun {
			r, err := a.Client.SecretCreate(ctx, spec)
			if err != nil {
				return nil, fmt.Errorf("create secret %s: %w", vname, err)
			}
			id = r.ID
		}
		out[s.Name] = versioned{id: id, name: vname}
		action := Created
		for _, l := range live {
			if l.Spec.Labels[api.LabelName] == s.Name && a.Owner.owns(l.Spec.Labels) {
				action = Configured
			}
		}
		res.add(KindSecret, s.Name, action, "")
	}
	return out, nil
}

// ---- services ----

// DesiredServiceSpec returns the spec that will be sent to Docker, with
// config and secret references resolved and labels added. lookup* are used
// for references to objects that are not part of the desired state.
func (a *Applier) DesiredServiceSpec(s *Service, configs, secrets map[string]versioned,
	liveConfigs map[string]swarm.Config, liveSecrets map[string]swarm.Secret) (swarm.ServiceSpec, error) {
	spec, err := deepCopy(s.Spec)
	if err != nil {
		return spec, err
	}
	spec.Name = s.Name
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, ref := range cs.Configs {
			v, ok := configs[ref.ConfigName]
			if !ok {
				cur, found := liveConfigs[ref.ConfigName]
				if !found {
					return spec, fmt.Errorf("service %s: config %q is neither in the source nor in Swarm", spec.Name, ref.ConfigName)
				}
				v = versioned{id: cur.ID, name: cur.Spec.Name}
			}
			ref.ConfigName, ref.ConfigID = v.name, v.id
		}
		for _, ref := range cs.Secrets {
			v, ok := secrets[ref.SecretName]
			if !ok {
				cur, found := liveSecrets[ref.SecretName]
				if !found {
					return spec, fmt.Errorf("service %s: secret %q is neither in the source nor in Swarm", spec.Name, ref.SecretName)
				}
				v = versioned{id: cur.ID, name: cur.Spec.Name}
			}
			ref.SecretName, ref.SecretID = v.name, v.id
		}
		if cs.Privileges != nil && cs.Privileges.CredentialSpec != nil && cs.Privileges.CredentialSpec.Config != "" {
			if v, ok := configs[cs.Privileges.CredentialSpec.Config]; ok {
				cs.Privileges.CredentialSpec.Config = v.id
			}
		}
	}
	labels := merge(spec.Labels, a.Owner.Labels())
	delete(labels, api.LabelSpecHash)
	spec.Labels = labels
	// Hash everything except IDs, which change between clusters and dry runs.
	h, err := specHash(spec)
	if err != nil {
		return spec, err
	}
	spec.Labels[api.LabelSpecHash] = h
	return spec, nil
}

func specHash(spec swarm.ServiceSpec) (string, error) {
	c, err := deepCopy(spec)
	if err != nil {
		return "", err
	}
	if cs := c.TaskTemplate.ContainerSpec; cs != nil {
		for _, r := range cs.Configs {
			r.ConfigID = ""
		}
		for _, r := range cs.Secrets {
			r.SecretID = ""
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16], nil
}

func (a *Applier) applyServices(ctx context.Context, desired []*Service, configs, secrets map[string]versioned, res *Result) error {
	live, err := a.Client.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return err
	}
	byName := map[string]swarm.Service{}
	for _, s := range live {
		byName[s.Spec.Name] = s
	}
	liveConfigs, liveSecrets := map[string]swarm.Config{}, map[string]swarm.Secret{}
	if needsLookup(desired, configs, secrets) {
		cl, err := a.Client.ConfigList(ctx, swarm.ConfigListOptions{})
		if err != nil {
			return err
		}
		for _, c := range cl {
			liveConfigs[c.Spec.Name] = c
		}
		sl, err := a.Client.SecretList(ctx, swarm.SecretListOptions{})
		if err != nil {
			return err
		}
		for _, s := range sl {
			liveSecrets[s.Spec.Name] = s
		}
	}

	for _, s := range desired {
		name := s.Name
		res.Inventory = append(res.Inventory, KindService+"/"+name)
		spec, err := a.DesiredServiceSpec(s, configs, secrets, liveConfigs, liveSecrets)
		if err != nil {
			return err
		}
		image := ""
		if spec.TaskTemplate.ContainerSpec != nil {
			image = spec.TaskTemplate.ContainerSpec.Image
		}
		auths := a.RegistryAuth
		if len(s.RegistryAuth) > 0 {
			auths = merge2(a.RegistryAuth, s.RegistryAuth)
		}
		encodedAuth := tfdocker.EncodedAuthForImage(image, auths)

		cur, exists := byName[name]
		if !exists {
			id := "dry-run"
			if !a.DryRun {
				r, err := a.Client.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{
					EncodedRegistryAuth: encodedAuth,
					QueryRegistry:       true,
				})
				if err != nil {
					return fmt.Errorf("create service %s: %w", name, err)
				}
				for _, w := range r.Warnings {
					a.log().Warn("service create", "service", name, "warning", w)
				}
				id = r.ID
			}
			res.ServiceIDs[name] = id
			res.add(KindService, name, Created, "")
			continue
		}
		res.ServiceIDs[name] = cur.ID
		reason := drift(cur.Spec, spec)
		if reason == "" {
			res.add(KindService, name, Unchanged, "")
			continue
		}
		if !a.DryRun {
			r, err := a.Client.ServiceUpdate(ctx, cur.ID, cur.Version, spec, swarm.ServiceUpdateOptions{
				EncodedRegistryAuth: encodedAuth,
				QueryRegistry:       true,
			})
			if err != nil && a.Force && !tfdocker.ContainsIgnorableErrorMessage(err.Error(), "update out of sequence") {
				a.log().Warn("update failed, recreating service (force)", "service", name, "error", err)
				if err := a.Client.ServiceRemove(ctx, cur.ID); err != nil {
					return fmt.Errorf("force recreate service %s: %w", name, err)
				}
				cr, err := a.Client.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{EncodedRegistryAuth: encodedAuth, QueryRegistry: true})
				if err != nil {
					return fmt.Errorf("force recreate service %s: %w", name, err)
				}
				res.ServiceIDs[name] = cr.ID
				res.add(KindService, name, Configured, "recreated: "+reason)
				continue
			}
			if err != nil {
				return fmt.Errorf("update service %s: %w", name, err)
			}
			for _, w := range r.Warnings {
				a.log().Warn("service update", "service", name, "warning", w)
			}
		}
		res.add(KindService, name, Configured, reason)
	}
	return nil
}

func needsLookup(desired []*Service, configs, secrets map[string]versioned) bool {
	for _, s := range desired {
		cs := s.Spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		for _, r := range cs.Configs {
			if _, ok := configs[r.ConfigName]; !ok {
				return true
			}
		}
		for _, r := range cs.Secrets {
			if _, ok := secrets[r.SecretName]; !ok {
				return true
			}
		}
	}
	return false
}

// drift explains why the live spec must be updated, or returns "".
func drift(live, want swarm.ServiceSpec) string {
	if live.Labels[api.LabelSpecHash] != want.Labels[api.LabelSpecHash] {
		if live.Labels[api.LabelSpecHash] == "" {
			return "adopting"
		}
		return "spec changed"
	}
	for k, v := range want.Labels {
		if live.Labels[k] != v {
			return "label " + k + " changed"
		}
	}
	if lc, wc := live.TaskTemplate.ContainerSpec, want.TaskTemplate.ContainerSpec; lc != nil && wc != nil {
		if !sameImage(lc.Image, wc.Image) {
			return fmt.Sprintf("image drifted to %s", lc.Image)
		}
	}
	if want.Mode.Replicated != nil && want.Mode.Replicated.Replicas != nil {
		if live.Mode.Replicated == nil || live.Mode.Replicated.Replicas == nil ||
			*live.Mode.Replicated.Replicas != *want.Mode.Replicated.Replicas {
			return "replicas drifted"
		}
	}
	return ""
}

// sameImage compares images, ignoring the digest Docker pins on create
// when the desired image has none.
func sameImage(live, want string) bool {
	if live == want {
		return true
	}
	if !strings.Contains(want, "@") {
		if i := strings.Index(live, "@"); i >= 0 {
			live = live[:i]
		}
	}
	return normalizeImage(live) == normalizeImage(want)
}

func normalizeImage(s string) string {
	s = strings.TrimPrefix(s, "docker.io/library/")
	s = strings.TrimPrefix(s, "docker.io/")
	if !strings.Contains(s, "@") {
		last := s[strings.LastIndex(s, "/")+1:]
		if !strings.Contains(last, ":") {
			s += ":latest"
		}
	}
	return s
}

// ---- prune ----

// Prune removes objects owned by a.Owner that are not in the inventory of
// the last Apply. With removed=false only unused older versions of configs
// and secrets are removed.
func (a *Applier) Prune(ctx context.Context, last *Result, removed bool) ([]Change, error) {
	keep := map[string]bool{}
	current := map[string]bool{}
	if last != nil {
		for _, i := range last.Inventory {
			keep[i] = true
		}
		for _, v := range last.Versions {
			current[v] = true
		}
	}
	var changes []Change
	f := a.Owner.filter()

	services, err := a.Client.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	if removed {
		var remaining []swarm.Service
		for _, s := range services {
			if !a.Owner.owns(s.Spec.Labels) || keep[KindService+"/"+s.Spec.Name] {
				remaining = append(remaining, s)
				continue
			}
			if !a.DryRun {
				if err := a.Client.ServiceRemove(ctx, s.ID); err != nil {
					return changes, fmt.Errorf("delete service %s: %w", s.Spec.Name, err)
				}
			}
			changes = append(changes, Change{Kind: KindService, Name: s.Spec.Name, Action: Deleted})
		}
		services = remaining
	}

	usedConfigs, usedSecrets := map[string]bool{}, map[string]bool{}
	usedNetworks := map[string]bool{}
	for _, s := range services {
		for _, spec := range []*swarm.ServiceSpec{&s.Spec, s.PreviousSpec} {
			if spec == nil {
				continue
			}
			if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
				for _, r := range cs.Configs {
					usedConfigs[r.ConfigID] = true
				}
				for _, r := range cs.Secrets {
					usedSecrets[r.SecretID] = true
				}
				if cs.Privileges != nil && cs.Privileges.CredentialSpec != nil {
					usedConfigs[cs.Privileges.CredentialSpec.Config] = true
				}
			}
			for _, n := range spec.TaskTemplate.Networks {
				usedNetworks[n.Target] = true
			}
		}
		for _, n := range s.Spec.TaskTemplate.Networks {
			usedNetworks[n.Target] = true
		}
	}

	configs, err := a.Client.ConfigList(ctx, swarm.ConfigListOptions{Filters: f})
	if err != nil {
		return changes, err
	}
	for _, c := range configs {
		logical := c.Spec.Labels[api.LabelName]
		if usedConfigs[c.ID] || usedConfigs[c.Spec.Name] {
			continue
		}
		stillDesired := keep[KindConfig+"/"+logical]
		if !stillDesired && !removed {
			continue
		}
		if stillDesired && current[c.Spec.Name] {
			continue
		}
		if !a.DryRun {
			if err := a.Client.ConfigRemove(ctx, c.ID); err != nil {
				if tfdocker.ContainsIgnorableErrorMessage(err.Error(), "in use", "not found") {
					continue
				}
				return changes, fmt.Errorf("delete config %s: %w", c.Spec.Name, err)
			}
		}
		reason := "old version"
		if !stillDesired {
			reason = ""
		}
		changes = append(changes, Change{Kind: KindConfig, Name: c.Spec.Name, Action: Deleted, Reason: reason})
	}

	secrets, err := a.Client.SecretList(ctx, swarm.SecretListOptions{Filters: f})
	if err != nil {
		return changes, err
	}
	for _, s := range secrets {
		logical := s.Spec.Labels[api.LabelName]
		if usedSecrets[s.ID] || usedSecrets[s.Spec.Name] {
			continue
		}
		stillDesired := keep[KindSecret+"/"+logical]
		if !stillDesired && !removed {
			continue
		}
		if stillDesired && current[s.Spec.Name] {
			continue
		}
		if !a.DryRun {
			if err := a.Client.SecretRemove(ctx, s.ID); err != nil {
				if tfdocker.ContainsIgnorableErrorMessage(err.Error(), "in use", "not found") {
					continue
				}
				return changes, fmt.Errorf("delete secret %s: %w", s.Spec.Name, err)
			}
		}
		reason := "old version"
		if !stillDesired {
			reason = ""
		}
		changes = append(changes, Change{Kind: KindSecret, Name: s.Spec.Name, Action: Deleted, Reason: reason})
	}

	if removed {
		nets, err := a.Client.NetworkList(ctx, network.ListOptions{Filters: f})
		if err != nil {
			return changes, err
		}
		for _, n := range nets {
			if keep[KindNetwork+"/"+n.Name] || usedNetworks[n.ID] || usedNetworks[n.Name] {
				continue
			}
			if !a.DryRun {
				if err := a.Client.NetworkRemove(ctx, n.ID); err != nil {
					if tfdocker.ContainsIgnorableErrorMessage(err.Error(), "in use", "active endpoints", "not found") {
						a.log().Info("network still in use, will retry", "network", n.Name)
						continue
					}
					return changes, fmt.Errorf("delete network %s: %w", n.Name, err)
				}
			}
			changes = append(changes, Change{Kind: KindNetwork, Name: n.Name, Action: Deleted})
		}
	}
	return changes, nil
}

// Wait waits for each service in ids to converge, and each job to
// complete.
func (a *Applier) Wait(ctx context.Context, ids map[string]string, timeout time.Duration) error {
	if a.DryRun {
		return nil
	}
	deadline := time.Now().Add(timeout)
	names := make([]string, 0, len(ids))
	for n := range ids {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("timeout waiting for services")
		}
		id := ids[n]
		err := WaitHealthy(ctx, left, 2*time.Second, func(ctx context.Context) (Health, error) {
			return CheckService(ctx, a.Client, id)
		})
		if err != nil {
			return fmt.Errorf("service %s: %w", n, err)
		}
	}
	return nil
}

func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}

func deepCopy[T any](v T) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

func merge2(a, b map[string]registry.AuthConfig) map[string]registry.AuthConfig {
	out := map[string]registry.AuthConfig{}
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}
