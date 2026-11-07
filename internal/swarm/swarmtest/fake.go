// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package swarmtest provides an in-memory Docker Swarm API for tests.
package swarmtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
)

// Fake is an in-memory implementation of swarm.Client. It is safe for
// concurrent use. Its fields may be read and set directly between calls.
type Fake struct {
	mu sync.Mutex

	ClusterID string
	Services  []swarm.Service
	Configs   []swarm.Config
	Secrets   []swarm.Secret
	Networks  []network.Summary

	// Calls records mutating calls as "Method name", in order.
	Calls []string
	// UpdateErr, when set, is returned by ServiceUpdate.
	UpdateErr error

	seq int
}

// New returns an empty swarm with the cluster ID "test-cluster".
func New() *Fake { return &Fake{ClusterID: "test-cluster"} }

func (f *Fake) id(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s%d", prefix, f.seq)
}

func (f *Fake) record(method, name string) { f.Calls = append(f.Calls, method+" "+name) }

// TakeCalls returns the recorded mutating calls and clears them.
func (f *Fake) TakeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.Calls
	f.Calls = nil
	return c
}

// Service returns the service named name.
func (f *Fake) Service(name string) (swarm.Service, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.Services {
		if s.Spec.Name == name {
			return s, true
		}
	}
	return swarm.Service{}, false
}

// Names lists the names of all objects as "Kind/name", sorted.
func (f *Fake) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.Services {
		out = append(out, "Service/"+s.Spec.Name)
	}
	for _, c := range f.Configs {
		out = append(out, "Config/"+c.Spec.Name)
	}
	for _, s := range f.Secrets {
		out = append(out, "Secret/"+s.Spec.Name)
	}
	for _, n := range f.Networks {
		out = append(out, "Network/"+n.Name)
	}
	sort.Strings(out)
	return out
}

// matches implements the label filters the applier uses.
func matches(args filters.Args, labels map[string]string) bool {
	for _, l := range args.Get("label") {
		k, v, hasValue := strings.Cut(l, "=")
		got, ok := labels[k]
		if !ok || (hasValue && got != v) {
			return false
		}
	}
	return true
}

func (f *Fake) Info(context.Context) (system.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var info system.Info
	info.Swarm.LocalNodeState = swarm.LocalNodeStateActive
	info.Swarm.ControlAvailable = true
	info.Swarm.Cluster = &swarm.ClusterInfo{ID: f.ClusterID}
	return info, nil
}

func (f *Fake) ServiceList(_ context.Context, o swarm.ServiceListOptions) ([]swarm.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []swarm.Service
	for _, s := range f.Services {
		if matches(o.Filters, s.Spec.Labels) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *Fake) ServiceInspectWithRaw(_ context.Context, id string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.Services {
		if s.ID == id || s.Spec.Name == id {
			return s, nil, nil
		}
	}
	return swarm.Service{}, nil, fmt.Errorf("service %s not found", id)
}

func (f *Fake) ServiceCreate(_ context.Context, spec swarm.ServiceSpec, _ swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.Services {
		if s.Spec.Name == spec.Name {
			return swarm.ServiceCreateResponse{}, fmt.Errorf("service %s already exists", spec.Name)
		}
	}
	id := f.id("svc")
	f.Services = append(f.Services, swarm.Service{ID: id, Meta: swarm.Meta{Version: swarm.Version{Index: 1}}, Spec: spec})
	f.record("ServiceCreate", spec.Name)
	return swarm.ServiceCreateResponse{ID: id}, nil
}

func (f *Fake) ServiceUpdate(_ context.Context, id string, v swarm.Version, spec swarm.ServiceSpec, _ swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UpdateErr != nil {
		return swarm.ServiceUpdateResponse{}, f.UpdateErr
	}
	for i, s := range f.Services {
		if s.ID != id {
			continue
		}
		if s.Version != v {
			return swarm.ServiceUpdateResponse{}, errors.New("update out of sequence")
		}
		prev := s.Spec
		s.PreviousSpec = &prev
		s.Spec = spec
		s.Version.Index++
		f.Services[i] = s
		f.record("ServiceUpdate", spec.Name)
		return swarm.ServiceUpdateResponse{}, nil
	}
	return swarm.ServiceUpdateResponse{}, fmt.Errorf("service %s not found", id)
}

func (f *Fake) ServiceRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.Services {
		if s.ID == id {
			f.Services = append(f.Services[:i], f.Services[i+1:]...)
			f.record("ServiceRemove", s.Spec.Name)
			return nil
		}
	}
	return fmt.Errorf("service %s not found", id)
}

func (f *Fake) TaskList(context.Context, swarm.TaskListOptions) ([]swarm.Task, error) {
	return nil, nil
}

func (f *Fake) NodeList(context.Context, swarm.NodeListOptions) ([]swarm.Node, error) {
	return nil, nil
}

func (f *Fake) ConfigList(_ context.Context, o swarm.ConfigListOptions) ([]swarm.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []swarm.Config
	for _, c := range f.Configs {
		if matches(o.Filters, c.Spec.Labels) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *Fake) ConfigCreate(_ context.Context, spec swarm.ConfigSpec) (swarm.ConfigCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Configs {
		if c.Spec.Name == spec.Name {
			return swarm.ConfigCreateResponse{}, fmt.Errorf("config %s already exists", spec.Name)
		}
	}
	id := f.id("cfg")
	f.Configs = append(f.Configs, swarm.Config{ID: id, Spec: spec})
	f.record("ConfigCreate", spec.Name)
	return swarm.ConfigCreateResponse{ID: id}, nil
}

func (f *Fake) inUse(id string, ref func(*swarm.ContainerSpec) []string) bool {
	for _, s := range f.Services {
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil {
			for _, r := range ref(cs) {
				if r == id {
					return true
				}
			}
		}
	}
	return false
}

func configIDs(cs *swarm.ContainerSpec) []string {
	var out []string
	for _, r := range cs.Configs {
		out = append(out, r.ConfigID)
	}
	return out
}

func secretIDs(cs *swarm.ContainerSpec) []string {
	var out []string
	for _, r := range cs.Secrets {
		out = append(out, r.SecretID)
	}
	return out
}

func (f *Fake) ConfigRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inUse(id, configIDs) {
		return fmt.Errorf("config %s is in use", id)
	}
	for i, c := range f.Configs {
		if c.ID == id {
			f.Configs = append(f.Configs[:i], f.Configs[i+1:]...)
			f.record("ConfigRemove", c.Spec.Name)
			return nil
		}
	}
	return fmt.Errorf("config %s not found", id)
}

func (f *Fake) SecretList(_ context.Context, o swarm.SecretListOptions) ([]swarm.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []swarm.Secret
	for _, s := range f.Secrets {
		if matches(o.Filters, s.Spec.Labels) {
			// Like Docker, never return secret data.
			s.Spec.Data = nil
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *Fake) SecretCreate(_ context.Context, spec swarm.SecretSpec) (swarm.SecretCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.Secrets {
		if s.Spec.Name == spec.Name {
			return swarm.SecretCreateResponse{}, fmt.Errorf("secret %s already exists", spec.Name)
		}
	}
	id := f.id("sec")
	f.Secrets = append(f.Secrets, swarm.Secret{ID: id, Spec: spec})
	f.record("SecretCreate", spec.Name)
	return swarm.SecretCreateResponse{ID: id}, nil
}

func (f *Fake) SecretRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inUse(id, secretIDs) {
		return fmt.Errorf("secret %s is in use", id)
	}
	for i, s := range f.Secrets {
		if s.ID == id {
			f.Secrets = append(f.Secrets[:i], f.Secrets[i+1:]...)
			f.record("SecretRemove", s.Spec.Name)
			return nil
		}
	}
	return fmt.Errorf("secret %s not found", id)
}

func (f *Fake) NetworkList(_ context.Context, o network.ListOptions) ([]network.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []network.Summary
	for _, n := range f.Networks {
		if matches(o.Filters, n.Labels) {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *Fake) NetworkCreate(_ context.Context, name string, o network.CreateOptions) (network.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.Networks {
		if n.Name == name {
			return network.CreateResponse{}, fmt.Errorf("network %s already exists", name)
		}
	}
	id := f.id("net")
	f.Networks = append(f.Networks, network.Summary{ID: id, Name: name, Driver: o.Driver, Scope: o.Scope, Labels: o.Labels})
	f.record("NetworkCreate", name)
	return network.CreateResponse{ID: id}, nil
}

func (f *Fake) NetworkRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, n := range f.Networks {
		if n.ID == id {
			for _, s := range f.Services {
				for _, a := range s.Spec.TaskTemplate.Networks {
					if a.Target == n.ID || a.Target == n.Name {
						return fmt.Errorf("network %s has active endpoints", n.Name)
					}
				}
			}
			f.Networks = append(f.Networks[:i], f.Networks[i+1:]...)
			f.record("NetworkRemove", n.Name)
			return nil
		}
	}
	return fmt.Errorf("network %s not found", id)
}
