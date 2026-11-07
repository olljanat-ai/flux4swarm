// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package importer exports a running Swarm as a Flux2 compatible Git
// layout of plain Kubernetes manifests: Deployments/DaemonSets/Jobs,
// Services, ConfigMaps, PersistentVolumeClaims, ExternalSecrets (Docker
// never returns secret values) and Gateway API HTTPRoutes from Traefik
// labels. Whatever has no Kubernetes equivalent is kept in the
// swarm.flux4swarm.io/service-spec annotation, so reconciling the export
// gives back the same Swarm services.
package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/render"
	swarmapply "github.com/olljanat-ai/flux4swarm/internal/swarm"
	"github.com/olljanat-ai/flux4swarm/internal/traefik"
	"github.com/olljanat-ai/flux4swarm/internal/translate"
)

// Client is the Docker API subset the importer needs.
type Client interface {
	ServiceList(ctx context.Context, options swarm.ServiceListOptions) ([]swarm.Service, error)
	ConfigList(ctx context.Context, options swarm.ConfigListOptions) ([]swarm.Config, error)
	ConfigInspectWithRaw(ctx context.Context, id string) (swarm.Config, []byte, error)
	SecretList(ctx context.Context, options swarm.SecretListOptions) ([]swarm.Secret, error)
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
}

// Secret store providers for the generated SecretStore.
const (
	ProviderFake    = "fake"
	ProviderVault   = "vault"
	ProviderAzureKV = "azurekv"
)

// Options configures an export.
type Options struct {
	// Cluster names the cluster directory (clusters/<cluster>).
	Cluster string
	// Path overrides the cluster directory, relative to the repository root.
	Path string
	// Bootstrap writes flux-system/gotk-sync.yaml and kustomization.yaml.
	Bootstrap bool
	// Adopt keeps the image digests that run now, so the first reconcile
	// does not pull newer images for the same tags.
	Adopt bool
	// GitURL, Branch, GitSecret and GitProvider go into the bootstrap
	// GitRepository.
	GitURL      string
	Branch      string
	GitSecret   string
	GitProvider string
	// SecretStore names the store ExternalSecrets point at.
	SecretStore     string
	SecretStoreKind string
	// SecretStoreProvider selects the SecretStore template.
	SecretStoreProvider string
	// SecretKeyPrefix is prepended to remoteRef.key.
	SecretKeyPrefix string
	// Exclude skips objects whose Swarm name matches.
	Exclude *regexp.Regexp
	// SecretEnvNames adds patterns to DefaultSecretEnvNames. An env var
	// whose name contains a pattern (case-insensitive) is exported as an
	// ExternalSecret reference instead of a plain value.
	SecretEnvNames []string
	// NoDefaultSecretEnvNames drops DefaultSecretEnvNames, so only
	// SecretEnvNames are used; with none, all env values are exported as is.
	NoDefaultSecretEnvNames bool
}

// DefaultSecretEnvNames are the well-known env var name parts that hold
// secrets. An env var whose name contains one (case-insensitive) is
// exported as an ExternalSecret reference, so its value stays out of Git.
var DefaultSecretEnvNames = []string{
	"PASSWORD", "PASSWD", "SECRET", "TOKEN", "API_KEY", "APIKEY",
	"ACCESS_KEY", "PRIVATE_KEY", "CREDENTIAL", "CONNECTION_STRING", "CONNECTIONSTRING",
}

// secretEnvNotSuffixes are name endings that point at a secret instead of
// holding it (MYSQL_PASSWORD_FILE=/run/secrets/db), so they stay plain.
var secretEnvNotSuffixes = []string{"_FILE", "_PATH", "_DIR"}

// secretEnvPatterns returns the effective, upper-cased pattern list.
func (o Options) secretEnvPatterns() []string {
	var out []string
	if !o.NoDefaultSecretEnvNames {
		out = append(out, DefaultSecretEnvNames...)
	}
	for _, p := range o.SecretEnvNames {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToUpper(p))
		}
	}
	return out
}

// isSecretEnv reports whether env var name/value should go to the secret store.
func isSecretEnv(patterns []string, name, value string) bool {
	if value == "" || strings.Contains(value, "{{") {
		return false
	}
	n := strings.ToUpper(name)
	for _, s := range secretEnvNotSuffixes {
		if strings.HasSuffix(n, s) {
			return false
		}
	}
	for _, p := range patterns {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

// Result is the set of files to write, keyed by slash-separated path
// relative to the repository root.
type Result struct {
	Files map[string][]byte
	// Root is the cluster directory.
	Root string
	// Clean are directories the export owns; they are emptied before
	// writing so that removed objects disappear.
	Clean []string
	Notes []string
	// Skipped lists the Swarm objects that were not exported, and why.
	Skipped []Skipped
	// Desired is the Swarm state the export renders to, for AdoptLabels.
	Desired *swarmapply.Desired
	// Report is the Kubernetes migration readiness report, also written
	// to <root>/MIGRATION.md.
	Report *Report
}

// Skipped is a Swarm object that was not exported.
type Skipped struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func (s Skipped) String() string { return s.Kind + "/" + s.Name + " (" + s.Reason + ")" }

// Reasons for Skipped.
const (
	SkippedExcluded = "matches --exclude"
	SkippedUnused   = "unused version of a flux4swarm managed object"
)

func (e *exporter) skipped(kind, name, reason string) {
	e.res.Skipped = append(e.res.Skipped, Skipped{Kind: kind, Name: name, Reason: reason})
}

// AppsDir is the directory, relative to the cluster directory, with the
// application manifests.
const AppsDir = "apps"

// Labels flux4swarm adds itself; they are not exported.
var ownLabels = []string{
	api.LabelKustomizeName, api.LabelKustomizeNamespace, api.LabelManagedBy,
	api.LabelName, api.LabelSpecHash, api.LabelExternalSecret,
}

func (o *Options) defaults() error {
	if o.Cluster == "" {
		o.Cluster = "default"
	}
	if o.Path == "" {
		o.Path = "clusters/" + o.Cluster
	}
	o.Path = strings.Trim(path.Clean(strings.TrimPrefix(o.Path, "./")), "/")
	if o.Path == "." || o.Path == "" || strings.HasPrefix(o.Path, "..") {
		return fmt.Errorf("invalid path %q", o.Path)
	}
	if o.Branch == "" {
		o.Branch = "main"
	}
	if o.SecretStore == "" {
		o.SecretStore = "default"
	}
	if o.SecretStoreKind == "" {
		o.SecretStoreKind = api.KindSecretStore
	}
	if o.SecretStoreKind != api.KindSecretStore && o.SecretStoreKind != api.KindClusterSecretStore {
		return fmt.Errorf("secret store kind must be SecretStore or ClusterSecretStore")
	}
	if o.SecretStoreProvider == "" {
		o.SecretStoreProvider = ProviderVault
	}
	switch o.SecretStoreProvider {
	case ProviderFake, ProviderVault, ProviderAzureKV:
	default:
		return fmt.Errorf("unsupported secret store provider %q (fake, vault, azurekv)", o.SecretStoreProvider)
	}
	if o.GitSecret == "" {
		o.GitSecret = "flux-system"
	}
	return nil
}

func (o *Options) skip(name string) bool {
	return o.Exclude != nil && o.Exclude.MatchString(name)
}

// exporter holds the state of one export.
type exporter struct {
	o                           Options
	res                         *Result
	set                         *api.Set
	files                       map[string][]any // file -> objects
	notes                       []string
	nsUsed                      map[string]bool
	original                    map[string]swarm.ServiceSpec // swarm name -> cleaned spec
	workload                    map[string]*metav1.ObjectMeta
	podSpecs                    map[string]*corev1.PodSpec
	cmKeys                      map[string]string // docker config logical name -> ns/cm/key
	cms                         map[string]*corev1.ConfigMap
	extSecrets                  map[string]*api.ExternalSecret // ns/name
	placeholders                []*corev1.Secret
	secretEnv                   []string
	pvcs                        map[string]*corev1.PersistentVolumeClaim
	services                    []reportService
	entryPoints, tlsEntryPoints []string
}

// Export reads the Swarm and renders the manifests.
func Export(ctx context.Context, c Client, o Options) (*Result, error) {
	if err := o.defaults(); err != nil {
		return nil, err
	}
	e := &exporter{
		o:          o,
		res:        &Result{Files: map[string][]byte{}, Root: o.Path},
		set:        &api.Set{},
		files:      map[string][]any{},
		nsUsed:     map[string]bool{},
		original:   map[string]swarm.ServiceSpec{},
		workload:   map[string]*metav1.ObjectMeta{},
		podSpecs:   map[string]*corev1.PodSpec{},
		cmKeys:     map[string]string{},
		cms:        map[string]*corev1.ConfigMap{},
		extSecrets: map[string]*api.ExternalSecret{},
		secretEnv:  o.secretEnvPatterns(),
		pvcs:       map[string]*corev1.PersistentVolumeClaim{},
	}
	e.res.Clean = []string{o.Path + "/" + AppsDir}
	if err := e.run(ctx, c); err != nil {
		return nil, err
	}
	return e.res, nil
}

func (e *exporter) add(file string, obj any) {
	e.files[file] = append(e.files[file], obj)
}

func (e *exporter) nsDir(ns string) string {
	e.nsUsed[ns] = true
	return e.o.Path + "/" + AppsDir + "/" + ns
}

func (e *exporter) run(ctx context.Context, c Client) error {
	nets, err := c.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return err
	}
	netNames := map[string]string{}
	for _, n := range nets {
		netNames[n.ID] = n.Name
	}
	services, err := c.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return err
	}
	configs, err := c.ConfigList(ctx, swarm.ConfigListOptions{})
	if err != nil {
		return err
	}
	secrets, err := c.SecretList(ctx, swarm.SecretListOptions{})
	if err != nil {
		return err
	}
	configByID := map[string]swarm.Config{}
	for _, cf := range configs {
		full, _, err := c.ConfigInspectWithRaw(ctx, cf.ID)
		if err != nil {
			return err
		}
		configByID[cf.ID] = full
	}
	secretByID := map[string]swarm.Secret{}
	for _, s := range secrets {
		secretByID[s.ID] = s
	}

	sort.Slice(services, func(i, j int) bool { return services[i].Spec.Name < services[j].Spec.Name })
	// Middlewares may be declared on one service and used by another.
	allMW := map[string]map[string]string{}
	for _, s := range services {
		for k, v := range traefik.Parse(s.Spec.Labels).Middlewares {
			allMW[k] = v
		}
	}
	for _, s := range services {
		if e.o.skip(s.Spec.Name) {
			e.skipped(swarmapply.KindService, s.Spec.Name, SkippedExcluded)
			continue
		}
		if err := e.service(s, netNames, configByID, secretByID, allMW); err != nil {
			return fmt.Errorf("service %s: %w", s.Spec.Name, err)
		}
	}
	// Configs and secrets that no service uses.
	for _, cf := range configs {
		cf := configByID[cf.ID]
		name := logicalName(cf.Spec.Name, cf.Spec.Labels)
		if _, done := e.cmKeys[name]; done {
			continue
		}
		if e.o.skip(name) {
			e.skipped(swarmapply.KindConfig, cf.Spec.Name, SkippedExcluded)
			continue
		}
		if cf.Spec.Labels[api.LabelName] != "" {
			e.skipped(swarmapply.KindConfig, cf.Spec.Name, SkippedUnused)
			continue
		}
		ns, short := splitNS(name, cf.Spec.Labels)
		e.configMap(ns, short, path.Base(short), cf)
	}
	usedSecrets := map[string]bool{}
	for _, svc := range services {
		if cs := svc.Spec.TaskTemplate.ContainerSpec; cs != nil && !e.o.skip(svc.Spec.Name) {
			for _, r := range cs.Secrets {
				usedSecrets[r.SecretID] = true
			}
		}
	}
	for _, s := range secrets {
		name := logicalName(s.Spec.Name, s.Spec.Labels)
		if e.o.skip(name) {
			e.skipped(swarmapply.KindSecret, s.Spec.Name, SkippedExcluded)
			continue
		}
		if s.Spec.Labels[api.LabelName] != "" {
			if !usedSecrets[s.ID] {
				e.skipped(swarmapply.KindSecret, s.Spec.Name, SkippedUnused)
			}
			continue
		}
		ns, short := splitNS(name, s.Spec.Labels)
		e.externalSecret(ns, dnsName(short), name, s.Spec.Labels)
	}
	if err := e.roundTrip(); err != nil {
		return err
	}
	if err := e.write(); err != nil {
		return err
	}
	e.report()
	return nil
}

// service converts one Swarm service.
func (e *exporter) service(s swarm.Service, netNames map[string]string, configs map[string]swarm.Config,
	secrets map[string]swarm.Secret, allMW map[string]map[string]string) error {
	spec, err := cleanService(s.Spec, netNames, configs, secrets, e.o.Adopt)
	if err != nil {
		return err
	}
	ns, short := splitNS(spec.Name, spec.Labels)
	name := dnsName(short)
	meta := metav1.ObjectMeta{Name: name, Namespace: ns, Labels: k8sLabels(spec.Labels)}
	if translate.SwarmName(ns, name) != spec.Name {
		meta.Annotations = map[string]string{api.AnnotationName: spec.Name}
	}
	selector := map[string]string{"app.kubernetes.io/name": name}
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return fmt.Errorf("only container services are supported")
	}
	ctr := corev1.Container{
		Name:       name,
		Image:      cs.Image,
		Command:    cs.Command,
		Args:       cs.Args,
		WorkingDir: cs.Dir,
		TTY:        cs.TTY,
		Stdin:      cs.OpenStdin,
	}
	envSecret := e.secretEnvName(ns, name)
	var envSecretValues map[string][]byte
	for _, kv := range cs.Env {
		k, v, _ := strings.Cut(kv, "=")
		if !isSecretEnv(e.secretEnv, k, v) || len(validation.IsEnvVarName(k)) > 0 {
			ctr.Env = append(ctr.Env, corev1.EnvVar{Name: k, Value: v})
			continue
		}
		if envSecretValues == nil {
			envSecretValues = map[string][]byte{}
		}
		if _, dup := envSecretValues[k]; dup {
			// Docker keeps the last value of a repeated name.
			for i := range ctr.Env {
				if ctr.Env[i].Name == k {
					ctr.Env = append(ctr.Env[:i], ctr.Env[i+1:]...)
					break
				}
			}
		}
		envSecretValues[k] = []byte(v)
		ctr.Env = append(ctr.Env, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: envSecret}, Key: k}}})
	}
	if envSecretValues != nil {
		e.envExternalSecret(ns, envSecret, spec.Name, envSecretValues, spec.Labels)
	}
	pod := corev1.PodSpec{}
	if cs.Hostname != "" && len(validation.IsDNS1123Label(cs.Hostname)) == 0 {
		pod.Hostname = cs.Hostname
	}
	if cs.StopGracePeriod != nil {
		sec := int64(cs.StopGracePeriod.Seconds())
		pod.TerminationGracePeriodSeconds = &sec
	}
	sc := &corev1.SecurityContext{}
	scSet := false
	if u, g, ok := numericUser(cs.User); ok {
		sc.RunAsUser = u
		sc.RunAsGroup = g
		scSet = true
	}
	if cs.ReadOnly {
		ro := true
		sc.ReadOnlyRootFilesystem = &ro
		scSet = true
	}
	if len(cs.CapabilityAdd)+len(cs.CapabilityDrop) > 0 {
		sc.Capabilities = &corev1.Capabilities{}
		for _, c := range cs.CapabilityAdd {
			sc.Capabilities.Add = append(sc.Capabilities.Add, corev1.Capability(strings.TrimPrefix(c, "CAP_")))
		}
		for _, c := range cs.CapabilityDrop {
			sc.Capabilities.Drop = append(sc.Capabilities.Drop, corev1.Capability(strings.TrimPrefix(c, "CAP_")))
		}
		scSet = true
	}
	if scSet {
		ctr.SecurityContext = sc
	}
	if hc := cs.Healthcheck; hc != nil && len(hc.Test) > 1 && hc.Test[0] != "NONE" {
		cmd := hc.Test[1:]
		if hc.Test[0] == "CMD-SHELL" {
			cmd = []string{"/bin/sh", "-c", strings.Join(hc.Test[1:], " ")}
		}
		p := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: cmd}}}
		if hc.Interval > 0 {
			p.PeriodSeconds = int32(hc.Interval / time.Second)
		}
		if hc.Timeout > 0 {
			p.TimeoutSeconds = int32(hc.Timeout / time.Second)
		}
		if hc.Retries > 0 {
			p.FailureThreshold = int32(hc.Retries)
		}
		if hc.StartPeriod > 0 {
			p.InitialDelaySeconds = int32(hc.StartPeriod / time.Second)
		}
		ctr.LivenessProbe = p
	}
	if r := spec.TaskTemplate.Resources; r != nil {
		if r.Limits != nil {
			ctr.Resources.Limits = quantities(r.Limits.NanoCPUs, r.Limits.MemoryBytes)
		}
		if r.Reservations != nil {
			ctr.Resources.Requests = quantities(r.Reservations.NanoCPUs, r.Reservations.MemoryBytes)
		}
	}
	if p := spec.TaskTemplate.Placement; p != nil {
		pod.NodeSelector = nodeSelector(p.Constraints)
	}
	if cs.DNSConfig != nil {
		pod.DNSConfig = &corev1.PodDNSConfig{Nameservers: cs.DNSConfig.Nameservers, Searches: cs.DNSConfig.Search}
		for _, o := range cs.DNSConfig.Options {
			k, v, ok := strings.Cut(o, ":")
			opt := corev1.PodDNSConfigOption{Name: k}
			if ok {
				opt.Value = &v
			}
			pod.DNSConfig.Options = append(pod.DNSConfig.Options, opt)
		}
	}
	for _, h := range cs.Hosts {
		f := strings.Fields(h)
		if len(f) >= 2 {
			ip, host := f[0], f[1]
			if strings.Contains(f[0], ":") && !strings.Contains(f[1], ":") && len(validation.IsDNS1123Subdomain(f[0])) == 0 {
				ip, host = f[1], f[0]
			}
			pod.HostAliases = append(pod.HostAliases, corev1.HostAlias{IP: ip, Hostnames: []string{host}})
		}
	}

	// Volumes.
	for i, m := range cs.Mounts {
		vname := fmt.Sprintf("vol-%d", i)
		vm := corev1.VolumeMount{Name: vname, MountPath: m.Target, ReadOnly: m.ReadOnly}
		switch m.Type {
		case mount.TypeVolume:
			if m.Source == "" {
				pod.Volumes = append(pod.Volumes, corev1.Volume{Name: vname, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
				break
			}
			if strings.Contains(m.Source, "{{") {
				e.note("service %s: templated volume %q is kept in the %s annotation", spec.Name, m.Source, api.AnnotationServiceSpec)
				continue
			}
			vns, vshort := splitVolumeNS(m.Source, ns)
			claim := dnsName(vshort)
			if translate.SwarmName(vns, claim) != m.Source || vns != ns {
				e.note("service %s: volume %q cannot keep its name in namespace %s; kept in the %s annotation", spec.Name, m.Source, ns, api.AnnotationServiceSpec)
				continue
			}
			vname = claim
			vm.Name = claim
			if m.VolumeOptions != nil && m.VolumeOptions.Subpath != "" {
				vm.SubPath = m.VolumeOptions.Subpath
			}
			pod.Volumes = appendVolume(pod.Volumes, corev1.Volume{Name: claim, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}})
			e.pvc(ns, claim, m.VolumeOptions)
		case mount.TypeBind:
			pod.Volumes = append(pod.Volumes, corev1.Volume{Name: vname, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: m.Source}}})
		case mount.TypeTmpfs:
			ed := &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}
			if m.TmpfsOptions != nil && m.TmpfsOptions.SizeBytes > 0 {
				ed.SizeLimit = resource.NewQuantity(m.TmpfsOptions.SizeBytes, resource.BinarySI)
			}
			pod.Volumes = append(pod.Volumes, corev1.Volume{Name: vname, VolumeSource: corev1.VolumeSource{EmptyDir: ed}})
		default:
			e.note("service %s: mount type %s is kept in the %s annotation", spec.Name, m.Type, api.AnnotationServiceSpec)
			continue
		}
		ctr.VolumeMounts = append(ctr.VolumeMounts, vm)
	}
	for _, r := range cs.Configs {
		if r.File == nil {
			continue
		}
		cf, ok := findConfig(configs, r.ConfigName)
		if !ok {
			return fmt.Errorf("config %s not found", r.ConfigName)
		}
		cns, cshort := splitNS(r.ConfigName, cf.Spec.Labels)
		if cns != ns {
			cns = ns
		}
		base := path.Base(r.File.Name)
		cmName, key := e.configMap(cns, cshort, base, cf)
		vname := "cm-" + cmName
		mode := int32(r.File.Mode)
		items := []corev1.KeyToPath{{Key: key, Path: base}}
		if mode != 0o644 {
			items[0].Mode = &mode
		}
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: uniqueVol(pod.Volumes, vname), VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cmName}, Items: items}}})
		ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: pod.Volumes[len(pod.Volumes)-1].Name, MountPath: r.File.Name, SubPath: base, ReadOnly: true})
	}
	for _, r := range cs.Secrets {
		if r.File == nil {
			continue
		}
		sec, ok := findSecret(secrets, r.SecretName)
		if !ok {
			return fmt.Errorf("secret %s not found", r.SecretName)
		}
		_, sshort := splitNS(r.SecretName, sec.Spec.Labels)
		sname, skey := dnsName(sshort), dnsName(sshort)
		if sec.Spec.Labels[api.LabelName] != "" {
			if sn, k, ok := strings.Cut(sshort, "_"); ok {
				sname, skey = dnsName(sn), k
			}
		}
		key := e.externalSecretData(ns, sname, logicalName(r.SecretName, sec.Spec.Labels), skey, sec.Spec.Labels)
		base := path.Base(r.File.Name)
		mode := int32(r.File.Mode)
		items := []corev1.KeyToPath{{Key: key, Path: base}}
		if mode != 0o644 {
			items[0].Mode = &mode
		}
		vname := uniqueVol(pod.Volumes, "secret-"+sname)
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: vname, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: sname, Items: items}}})
		ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: vname, MountPath: r.File.Name, SubPath: base, ReadOnly: true})
	}
	pod.Containers = []corev1.Container{ctr}

	// Networks.
	var netNamesUsed []string
	for _, n := range spec.TaskTemplate.Networks {
		netNamesUsed = append(netNamesUsed, n.Target)
	}
	if !(len(netNamesUsed) == 1 && netNamesUsed[0] == translate.NetworkName(ns)) {
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		meta.Annotations[api.AnnotationNetworks] = strings.Join(netNamesUsed, ",")
	}

	tpl := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: selector}, Spec: pod}
	var obj any
	file := e.nsDir(ns) + "/"
	switch {
	case spec.Mode.Global != nil:
		obj = &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: meta,
			Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: selector}, Template: tpl}}
		file += "daemonset-" + name + ".yaml"
	case spec.Mode.ReplicatedJob != nil || spec.Mode.GlobalJob != nil:
		j := &batchv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}, ObjectMeta: meta}
		tpl.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
		if rp := spec.TaskTemplate.RestartPolicy; rp != nil && rp.Condition == swarm.RestartPolicyConditionNone {
			tpl.Spec.RestartPolicy = corev1.RestartPolicyNever
		}
		j.Spec.Template = tpl
		if rj := spec.Mode.ReplicatedJob; rj != nil {
			j.Spec.Parallelism = i32p(rj.MaxConcurrent)
			j.Spec.Completions = i32p(rj.TotalCompletions)
		}
		if rp := spec.TaskTemplate.RestartPolicy; rp != nil && rp.MaxAttempts != nil {
			b := int32(*rp.MaxAttempts)
			j.Spec.BackoffLimit = &b
		}
		obj = j
		file += "job-" + name + ".yaml"
	default:
		d := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta,
			Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: selector}, Template: tpl}}
		if r := spec.Mode.Replicated; r != nil && r.Replicas != nil {
			n := int32(*r.Replicas)
			d.Spec.Replicas = &n
		}
		if uc := spec.UpdateConfig; uc != nil && uc.Order == swarm.UpdateOrderStartFirst && uc.Parallelism == 1 {
			d.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType}
		} else {
			zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
			d.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &zero, MaxUnavailable: &one}}
		}
		obj = d
		file += "deployment-" + name + ".yaml"
	}
	e.add(file, obj)
	e.addToSet(obj)
	e.services = append(e.services, reportService{swarm: spec.Name, kind: kindOf(obj), ns: ns, name: name})
	e.original[spec.Name] = spec
	e.workload[spec.Name] = objectMeta(obj)
	e.podSpecs[spec.Name] = podSpecOf(obj)

	parsed := traefik.Parse(spec.Labels)
	// Published ports -> LoadBalancer Service.
	if ep := spec.EndpointSpec; ep != nil && len(ep.Ports) > 0 {
		lbName := name
		if len(parsed.Routers) > 0 {
			lbName = name + "-public"
		}
		svc := &corev1.Service{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: metav1.ObjectMeta{Name: lbName, Namespace: ns},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Selector: selector},
		}
		for _, p := range ep.Ports {
			proto := corev1.ProtocolTCP
			switch p.Protocol {
			case swarm.PortConfigProtocolUDP:
				proto = corev1.ProtocolUDP
			case swarm.PortConfigProtocolSCTP:
				proto = corev1.ProtocolSCTP
			}
			if p.PublishMode == swarm.PortConfigPublishModeHost {
				svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			}
			pname := p.Name
			if pname == "" && len(ep.Ports) > 1 {
				pname = fmt.Sprintf("%s-%d", strings.ToLower(string(proto)), p.PublishedPort)
			}
			svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: pname, Protocol: proto,
				Port: int32(p.PublishedPort), TargetPort: intstr.FromInt32(int32(p.TargetPort))})
		}
		e.add(e.nsDir(ns)+"/service-"+svc.Name+".yaml", svc)
		e.set.Services = append(e.set.Services, svc)
	}

	// Traefik routers -> HTTPRoutes + ClusterIP Service.
	if len(parsed.Routers) > 0 {
		ir := traefik.Import(traefik.ImportInput{Namespace: ns, K8sService: name, Parsed: parsed, AllMiddlewares: allMW})
		for _, w := range ir.Warnings {
			e.note("service %s: %s", spec.Name, w)
		}
		if len(ir.Ports) > 0 {
			svc := &corev1.Service{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       corev1.ServiceSpec{Selector: selector},
			}
			for _, p := range ir.Ports {
				svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("http-%d", p), Port: p, TargetPort: intstr.FromInt32(p)})
			}
			e.add(e.nsDir(ns)+"/service-"+name+".yaml", svc)
			e.set.Services = append(e.set.Services, svc)
		}
		for _, r := range ir.Routes {
			e.add(e.nsDir(ns)+"/httproute-"+r.Name+".yaml", r)
			e.set.HTTPRoutes = append(e.set.HTTPRoutes, r)
		}
		for _, mw := range ir.Middlewares {
			if s := traefik.SecretUsers(mw); s != nil {
				// htpasswd lines go to the secret store, like other secrets.
				users := s.StringData["users"]
				key := e.externalSecretData(ns, s.Name, s.Name, "users", nil)
				_ = key
				e.placeholders = append(e.placeholders, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: ns},
					Data:       map[string][]byte{"users": []byte(users)},
				})
				e.note("Middleware %s/%s: basicAuth users moved to secret store key %q", ns, mw.Metadata.Name, e.o.SecretKeyPrefix+s.Name)
			}
			e.add(e.nsDir(ns)+"/middleware-"+mw.Metadata.Name+".yaml", mw)
			e.set.Middlewares = append(e.set.Middlewares, mw)
		}
		e.entryPoints = append(e.entryPoints, ir.EntryPoints...)
		e.tlsEntryPoints = append(e.tlsEntryPoints, ir.TLSEntryPoints...)
	}
	return nil
}

func (e *exporter) addToSet(obj any) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		e.set.Deployments = append(e.set.Deployments, o)
	case *appsv1.DaemonSet:
		e.set.DaemonSets = append(e.set.DaemonSets, o)
	case *batchv1.Job:
		e.set.Jobs = append(e.set.Jobs, o)
	}
}

func kindOf(obj any) string {
	switch obj.(type) {
	case *appsv1.DaemonSet:
		return "DaemonSet"
	case *batchv1.Job:
		return "Job"
	}
	return "Deployment"
}

func objectMeta(obj any) *metav1.ObjectMeta {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return &o.ObjectMeta
	case *appsv1.DaemonSet:
		return &o.ObjectMeta
	case *batchv1.Job:
		return &o.ObjectMeta
	}
	return nil
}

func podSpecOf(obj any) *corev1.PodSpec {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return &o.Spec.Template.Spec
	case *appsv1.DaemonSet:
		return &o.Spec.Template.Spec
	case *batchv1.Job:
		return &o.Spec.Template.Spec
	}
	return nil
}

// configMap returns the ConfigMap name and key that hold a Docker config,
// creating the ConfigMap on first use.
func (e *exporter) configMap(ns, short, key string, cf swarm.Config) (string, string) {
	logical := logicalName(cf.Spec.Name, cf.Spec.Labels)
	if v, ok := e.cmKeys[logical]; ok {
		parts := strings.SplitN(v, "/", 3)
		if parts[0] == ns {
			return parts[1], parts[2]
		}
	}
	name := dnsName(short)
	if cf.Spec.Labels[api.LabelName] != "" {
		// Created by flux4swarm from <configmap>_<key>.
		if cm, k, ok := strings.Cut(short, "_"); ok {
			name, key = dnsName(cm), k
		}
	}
	if len(validation.IsConfigMapKey(key)) > 0 {
		key = "data"
	}
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: k8sLabels(stripLabels(cf.Spec.Labels))},
	}
	if utf8.Valid(cf.Spec.Data) {
		cm.Data = map[string]string{key: string(cf.Spec.Data)}
	} else {
		cm.BinaryData = map[string][]byte{key: cf.Spec.Data}
	}
	if cf.Spec.Templating != nil && cf.Spec.Templating.Name != "" {
		e.note("config %s uses the %s template driver, which has no Kubernetes equivalent; it is exported as plain data", cf.Spec.Name, cf.Spec.Templating.Name)
	}
	e.cmKeys[logical] = ns + "/" + name + "/" + key
	if prev, ok := e.cms[ns+"/"+name]; ok {
		// Two Docker configs with the same short name: merge the keys.
		for k, v := range cm.Data {
			if prev.Data == nil {
				prev.Data = map[string]string{}
			}
			prev.Data[k] = v
		}
		for k, v := range cm.BinaryData {
			if prev.BinaryData == nil {
				prev.BinaryData = map[string][]byte{}
			}
			prev.BinaryData[k] = v
		}
		return name, key
	}
	e.cms[ns+"/"+name] = cm
	e.add(e.nsDir(ns)+"/configmap-"+name+".yaml", cm)
	e.set.ConfigMaps = append(e.set.ConfigMaps, cm)
	return name, key
}

// externalSecret describes a Docker secret as an ExternalSecret and returns
// the key in the resulting Secret.
func (e *exporter) externalSecret(ns, name, dockerName string, labels map[string]string) string {
	return e.externalSecretData(ns, name, dockerName, name, labels)
}

func (e *exporter) externalSecretData(ns, name, remote, key string, labels map[string]string) string {
	if es, ok := e.extSecrets[ns+"/"+name]; ok {
		return es.Spec.Data[0].SecretKey
	}
	rk := e.o.SecretKeyPrefix + remote
	if e.o.SecretStoreProvider == ProviderAzureKV {
		rk = azureName(rk)
	}
	if len(validation.IsConfigMapKey(key)) > 0 {
		key = "value"
	}
	es := &api.ExternalSecret{
		TypeMeta: api.TypeMeta{APIVersion: api.ESOGroupVersion, Kind: api.KindExternalSecret},
		Metadata: api.ObjectMeta{Name: name, Namespace: ns},
		Spec: api.ExternalSecretSpec{
			RefreshInterval: &api.Duration{Duration: time.Hour},
			SecretStoreRef:  api.SecretStoreRef{Name: e.o.SecretStore, Kind: e.o.SecretStoreKind},
			Target:          api.ExternalSecretTarget{Name: name},
			Data:            []api.ExternalSecretData{{SecretKey: key, RemoteRef: api.RemoteReference{Key: rk}}},
		},
	}
	if l := k8sLabels(stripLabels(labels)); len(l) > 0 {
		es.Spec.Target.Template = &api.ExternalSecretTemplate{}
		es.Spec.Target.Template.Metadata.Labels = l
	}
	e.extSecrets[ns+"/"+name] = es
	e.add(e.nsDir(ns)+"/externalsecret-"+name+".yaml", es)
	e.set.ExternalSecrets = append(e.set.ExternalSecrets, es)
	return key
}

// secretEnvName picks the name of the Secret that holds the secret env
// values of service ns/name: <name>-env, unless a Docker secret has it.
func (e *exporter) secretEnvName(ns, name string) string {
	n := name + "-env"
	for i := 2; e.extSecrets[ns+"/"+n] != nil; i++ {
		n = fmt.Sprintf("%s-env-%d", name, i)
	}
	return n
}

// envExternalSecret writes one ExternalSecret with one key per secret env
// var of a service. The values are kept only as a placeholder Secret for the
// round trip; they are never written.
func (e *exporter) envExternalSecret(ns, name, service string, values map[string][]byte, labels map[string]string) {
	es := &api.ExternalSecret{
		TypeMeta: api.TypeMeta{APIVersion: api.ESOGroupVersion, Kind: api.KindExternalSecret},
		Metadata: api.ObjectMeta{Name: name, Namespace: ns},
		Spec: api.ExternalSecretSpec{
			RefreshInterval: &api.Duration{Duration: time.Hour},
			SecretStoreRef:  api.SecretStoreRef{Name: e.o.SecretStore, Kind: e.o.SecretStoreKind},
			Target:          api.ExternalSecretTarget{Name: name},
		},
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var remote []string
	for _, k := range keys {
		rk := e.o.SecretKeyPrefix + service + "-" + k
		if e.o.SecretStoreProvider == ProviderAzureKV {
			rk = azureName(rk)
		}
		remote = append(remote, rk)
		es.Spec.Data = append(es.Spec.Data, api.ExternalSecretData{SecretKey: k, RemoteRef: api.RemoteReference{Key: rk}})
	}
	if l := k8sLabels(stripLabels(labels)); len(l) > 0 {
		es.Spec.Target.Template = &api.ExternalSecretTemplate{}
		es.Spec.Target.Template.Metadata.Labels = l
	}
	e.extSecrets[ns+"/"+name] = es
	e.add(e.nsDir(ns)+"/externalsecret-"+name+".yaml", es)
	e.set.ExternalSecrets = append(e.set.ExternalSecrets, es)
	e.placeholders = append(e.placeholders, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       values,
	})
	e.note("Service %s: env %s look like secrets, so their values are not exported: store the current values under secret store key(s) %s (ExternalSecret %s/%s). Change the name list with --secret-env-name or --no-default-secret-env-names.", service, strings.Join(keys, ", "), strings.Join(remote, ", "), ns, name)
}

func (e *exporter) pvc(ns, name string, vo *mount.VolumeOptions) {
	if _, ok := e.pvcs[ns+"/"+name]; ok {
		return
	}
	p := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
	}
	if vo != nil {
		ann := map[string]string{}
		if vo.DriverConfig != nil {
			if vo.DriverConfig.Name != "" {
				ann[api.AnnotationVolumeDriver] = vo.DriverConfig.Name
			}
			if len(vo.DriverConfig.Options) > 0 {
				b, _ := json.Marshal(vo.DriverConfig.Options)
				ann[api.AnnotationVolumeOptions] = string(b)
			}
		}
		if len(ann) > 0 {
			p.Annotations = ann
		}
		if l := k8sLabels(vo.Labels); len(l) > 0 {
			p.Labels = l
		}
	}
	e.pvcs[ns+"/"+name] = p
	e.add(e.nsDir(ns)+"/persistentvolumeclaim-"+name+".yaml", p)
	e.set.PVCs = append(e.set.PVCs, p)
	e.notes = appendOnce(e.notes, "PersistentVolumeClaims request a placeholder size of 1Gi; set the real size and storageClassName for Kubernetes.")
}

// roundTrip renders the exported manifests back to Swarm specs and stores
// whatever differs from the live service in the service-spec annotation.
func (e *exporter) roundTrip() error {
	secrets := append([]*corev1.Secret(nil), e.placeholders...)
	for _, es := range e.extSecrets {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: es.Spec.Target.Name, Namespace: es.Metadata.Namespace}, Data: map[string][]byte{}}
		for _, d := range es.Spec.Data {
			if _, ok := s.Data[d.SecretKey]; !ok {
				s.Data[d.SecretKey] = []byte{}
			}
		}
		exists := false
		for _, p := range e.placeholders {
			if p.Name == s.Name && p.Namespace == s.Namespace {
				exists = true
			}
		}
		if !exists {
			secrets = append(secrets, s)
		}
	}
	set := e.set
	if len(set.HTTPRoutes) > 0 {
		// Render with the Gateway write() adds, as the reconcile will.
		cp := *set
		_, gw := traefik.Gateway(e.entryPoints, e.tlsEntryPoints)
		cp.Gateways = append(append([]*gatewayv1.Gateway(nil), set.Gateways...), gw)
		set = &cp
	}
	out, err := render.Render(set, secrets)
	if err != nil {
		return fmt.Errorf("render exported manifests: %w", err)
	}
	for _, w := range out.Warnings {
		e.note("%s", w)
	}
	e.res.Desired = &out.Desired
	for _, svc := range out.Desired.Services {
		orig, ok := e.original[svc.Name]
		if !ok {
			continue
		}
		gen := svc.Spec
		normalize(&orig, &gen)
		patch, err := translate.CreatePatch(gen, orig)
		if err != nil {
			return err
		}
		if patch == nil {
			continue
		}
		check, err := translate.ApplyPatch(gen, patch)
		if err != nil {
			return fmt.Errorf("service %s: patch does not apply: %w", svc.Name, err)
		}
		if p2, _ := translate.CreatePatch(check, orig); p2 != nil {
			return fmt.Errorf("service %s: round trip is not lossless: %s", svc.Name, p2)
		}
		svc.Spec = check // what the reconcile renders from the annotation
		meta := e.workload[svc.Name]
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		meta.Annotations[api.AnnotationServiceSpec] = string(patch)
	}
	return nil
}

// normalize makes equivalent forms identical before diffing: config and
// secret references take the generated names (they are versioned on
// apply) and relative file targets become absolute.
func normalize(orig, gen *swarm.ServiceSpec) {
	oc, gc := orig.TaskTemplate.ContainerSpec, gen.TaskTemplate.ContainerSpec
	if oc == nil || gc == nil {
		return
	}
	for _, r := range oc.Secrets {
		if r.File != nil && !strings.HasPrefix(r.File.Name, "/") {
			r.File.Name = "/run/secrets/" + r.File.Name
		}
	}
	for _, r := range oc.Configs {
		if r.File != nil && !strings.HasPrefix(r.File.Name, "/") {
			r.File.Name = "/" + r.File.Name
		}
	}
	if len(oc.Configs) == len(gc.Configs) {
		for i := range oc.Configs {
			oc.Configs[i].ConfigName = gc.Configs[i].ConfigName
		}
	}
	if len(oc.Secrets) == len(gc.Secrets) {
		for i := range oc.Secrets {
			oc.Secrets[i].SecretName = gc.Secrets[i].SecretName
		}
	}
}

func (e *exporter) note(format string, args ...any) {
	e.notes = appendOnce(e.notes, fmt.Sprintf(format, args...))
}

func appendOnce(list []string, s string) []string {
	for _, l := range list {
		if l == s {
			return list
		}
	}
	return append(list, s)
}

const header = "# Generated by flux4swarm import.\n"

// write renders all files.
func (e *exporter) write() error {
	// Namespaces and secret stores.
	var nss []string
	for ns := range e.nsUsed {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	needStore := map[string]bool{}
	storeKeys := map[string][]string{}
	for _, es := range e.extSecrets {
		needStore[es.Metadata.Namespace] = true
		for _, d := range es.Spec.Data {
			storeKeys[es.Metadata.Namespace] = append(storeKeys[es.Metadata.Namespace], d.RemoteRef.Key)
		}
	}
	clusterStoreWritten := false
	for _, ns := range nss {
		dir := e.nsDir(ns)
		if ns != "default" {
			e.add(dir+"/namespace.yaml", &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}
		if needStore[ns] {
			keys := storeKeys[ns]
			sort.Strings(keys)
			if e.o.SecretStoreKind == api.KindClusterSecretStore {
				if !clusterStoreWritten {
					var all []string
					for _, k := range storeKeys {
						all = append(all, k...)
					}
					sort.Strings(all)
					e.add(e.o.Path+"/"+AppsDir+"/clustersecretstore.yaml", secretStore(e.o, "", all))
					clusterStoreWritten = true
				}
			} else {
				e.add(dir+"/secretstore.yaml", secretStore(e.o, ns, keys))
			}
		}
	}
	if len(e.extSecrets) > 0 {
		e.note("%d secret(s) exported as ExternalSecrets. Docker does not return secret values: store each value in the %s store under its remoteRef.key and fill in the SecretStore.", len(e.extSecrets), e.o.SecretStoreProvider)
	}
	if len(e.set.HTTPRoutes) > 0 {
		gc, gw := traefik.Gateway(e.entryPoints, e.tlsEntryPoints)
		e.add(e.o.Path+"/"+AppsDir+"/"+traefik.GatewayNamespace+"/gateway.yaml", gc)
		e.add(e.o.Path+"/"+AppsDir+"/"+traefik.GatewayNamespace+"/gateway.yaml", gw)
		if !e.nsUsed[traefik.GatewayNamespace] {
			e.nsUsed[traefik.GatewayNamespace] = true
			e.add(e.o.Path+"/"+AppsDir+"/"+traefik.GatewayNamespace+"/namespace.yaml",
				&corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: traefik.GatewayNamespace}})
		}
		for _, l := range gw.Spec.Listeners {
			if l.TLS != nil {
				e.note("Gateway listener %s uses TLS: create Secret %s/%s on Kubernetes (Traefik on Swarm keeps using its own certificates).", l.Name, traefik.GatewayNamespace, l.TLS.CertificateRefs[0].Name)
			}
		}
	}

	// Per-directory kustomization.yaml files.
	dirs := map[string][]string{}
	for f := range e.files {
		dirs[path.Dir(f)] = append(dirs[path.Dir(f)], path.Base(f))
	}
	appsRoot := e.o.Path + "/" + AppsDir
	var appDirs []string
	for d, files := range dirs {
		if d == appsRoot {
			continue
		}
		sort.Strings(files)
		if err := e.render(d+"/kustomization.yaml", kustomization(files)); err != nil {
			return err
		}
		appDirs = append(appDirs, strings.TrimPrefix(d, appsRoot+"/"))
	}
	sort.Strings(appDirs)
	rootRes := append([]string(nil), dirs[appsRoot]...)
	sort.Strings(rootRes)
	rootRes = append(rootRes, appDirs...)
	// Always written, even for an empty Swarm: overlays outside apps/
	// list it as a resource and must keep building.
	if rootRes == nil {
		rootRes = []string{}
	}
	if err := e.render(appsRoot+"/kustomization.yaml", kustomization(rootRes)); err != nil {
		return err
	}
	for f, objs := range e.files {
		if err := e.render(f, objs...); err != nil {
			return err
		}
	}

	if e.o.Bootstrap {
		if err := e.bootstrap(); err != nil {
			return err
		}
	}
	e.res.Notes = e.notes
	return nil
}

func (e *exporter) render(file string, objs ...any) error {
	b, err := api.MarshalDocs(objs...)
	if err != nil {
		return err
	}
	b = cleanYAML(b)
	e.res.Files[file] = append([]byte(header), b...)
	return nil
}

// cleanYAML drops the empty fields Kubernetes types always marshal:
// status, creationTimestamp: null, resources: {} and strategy: {}.
func cleanYAML(b []byte) []byte {
	var out [][]byte
	for _, doc := range api.SplitYAML(b) {
		var m map[string]any
		if err := yaml.Unmarshal(doc, &m); err != nil {
			out = append(out, doc)
			continue
		}
		delete(m, "status")
		prune(m)
		c, err := yaml.Marshal(m)
		if err != nil {
			out = append(out, doc)
			continue
		}
		out = append(out, c)
	}
	return bytes.Join(out, []byte("---\n"))
}

func prune(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "creationTimestamp" && val == nil {
				delete(t, k)
				continue
			}
			if mm, ok := val.(map[string]any); ok && len(mm) == 0 && (k == "resources" || k == "strategy") {
				delete(t, k)
				continue
			}
			prune(val)
		}
	case []any:
		for _, e := range t {
			prune(e)
		}
	}
}

func kustomization(resources []string) map[string]any {
	return map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  resources,
	}
}

func (e *exporter) bootstrap() error {
	gr := &api.GitRepository{
		TypeMeta: api.TypeMeta{APIVersion: api.SourceGroupVersion, Kind: api.KindGitRepository},
		Metadata: api.ObjectMeta{Name: "flux-system", Namespace: api.DefaultNamespace},
		Spec: api.GitRepositorySpec{
			URL:       e.o.GitURL,
			Interval:  api.Duration{Duration: time.Minute},
			Ref:       &api.GitRepositoryRef{Branch: e.o.Branch},
			SecretRef: &api.LocalObjectReference{Name: e.o.GitSecret},
			Provider:  e.o.GitProvider,
		},
	}
	if gr.Spec.URL == "" {
		gr.Spec.URL = "https://github.com/OWNER/REPOSITORY"
		e.notes = append(e.notes, "No --git-url given: set spec.url in "+e.o.Path+"/flux-system/gotk-sync.yaml.")
	}
	timeout := api.Duration{Duration: 5 * time.Minute}
	ks := &api.Kustomization{
		TypeMeta: api.TypeMeta{APIVersion: api.KustomizeGroupVersion, Kind: api.KindKustomization},
		Metadata: api.ObjectMeta{Name: "flux-system", Namespace: api.DefaultNamespace},
		Spec: api.KustomizationSpec{
			Interval:  api.Duration{Duration: 10 * time.Minute},
			Path:      "./" + e.o.Path,
			Prune:     true,
			Wait:      true,
			Timeout:   &timeout,
			SourceRef: api.CrossNamespaceObjectReference{Kind: api.KindGitRepository, Name: "flux-system"},
		},
	}
	dir := e.o.Path + "/flux-system"
	if err := e.render(dir+"/gotk-sync.yaml", gr, ks); err != nil {
		return err
	}
	if err := e.render(dir+"/kustomization.yaml", kustomization([]string{"gotk-sync.yaml"})); err != nil {
		return err
	}
	e.notes = append(e.notes, "On Kubernetes run `flux install --export > "+dir+"/gotk-components.yaml` (or `flux bootstrap`) and add it to "+dir+"/kustomization.yaml.")
	return nil
}

func cleanService(in swarm.ServiceSpec, netNames map[string]string, configs map[string]swarm.Config, secrets map[string]swarm.Secret, adopt bool) (swarm.ServiceSpec, error) {
	var spec swarm.ServiceSpec
	b, err := json.Marshal(in)
	if err != nil {
		return spec, err
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return spec, err
	}
	spec.Labels = stripLabels(spec.Labels)
	delete(spec.Labels, "com.docker.stack.image")
	if len(spec.Labels) == 0 {
		spec.Labels = nil
	}
	spec.TaskTemplate.ForceUpdate = 0
	if spec.TaskTemplate.Runtime == swarm.RuntimeContainer {
		spec.TaskTemplate.Runtime = ""
	}
	if p := spec.TaskTemplate.Placement; p != nil {
		// Filled in by the daemon from the image manifest.
		p.Platforms = nil
		for i, c := range p.Constraints {
			if k, v, ok := strings.Cut(c, "=="); ok {
				p.Constraints[i] = strings.TrimSpace(k) + "==" + strings.TrimSpace(v)
			}
		}
		if len(p.Constraints) == 0 && len(p.Preferences) == 0 && p.MaxReplicas == 0 {
			spec.TaskTemplate.Placement = nil
		}
	}
	//nolint:staticcheck // Spec.Networks is deprecated; move it to the task template.
	if len(spec.Networks) > 0 {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, spec.Networks...)
		spec.Networks = nil
	}
	for i, n := range spec.TaskTemplate.Networks {
		if name, ok := netNames[n.Target]; ok {
			spec.TaskTemplate.Networks[i].Target = name
		}
	}
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		if !adopt {
			if i := strings.Index(cs.Image, "@sha256:"); i > 0 {
				cs.Image = cs.Image[:i]
			}
		}
		cs.Labels = stripLabels(cs.Labels)
		if cs.Isolation == "default" {
			cs.Isolation = ""
		}
		if p := cs.Privileges; p != nil && p.CredentialSpec == nil && p.SELinuxContext == nil && !p.NoNewPrivileges &&
			p.Seccomp == nil && p.AppArmor == nil {
			cs.Privileges = nil
		}
		for _, r := range cs.Configs {
			if c, ok := configs[r.ConfigID]; ok {
				r.ConfigName = c.Spec.Name
			}
			r.ConfigID = ""
			if r.File != nil && !strings.HasPrefix(r.File.Name, "/") {
				r.File.Name = "/" + r.File.Name
			}
		}
		for _, r := range cs.Secrets {
			if s, ok := secrets[r.SecretID]; ok {
				r.SecretName = s.Spec.Name
			}
			r.SecretID = ""
			if r.File != nil && !strings.HasPrefix(r.File.Name, "/") {
				r.File.Name = "/run/secrets/" + r.File.Name
			}
		}
	}
	return spec, nil
}

func findConfig(configs map[string]swarm.Config, name string) (swarm.Config, bool) {
	for _, c := range configs {
		if c.Spec.Name == name {
			return c, true
		}
	}
	return swarm.Config{}, false
}

func findSecret(secrets map[string]swarm.Secret, name string) (swarm.Secret, bool) {
	for _, s := range secrets {
		if s.Spec.Name == name {
			return s, true
		}
	}
	return swarm.Secret{}, false
}

func logicalName(name string, labels map[string]string) string {
	if l := labels[api.LabelName]; l != "" {
		return l
	}
	return name
}

// splitNS returns the Kubernetes namespace (Docker stack) and the name
// without the stack prefix.
func splitNS(name string, labels map[string]string) (string, string) {
	name = logicalName(name, labels)
	if ns := labels[api.LabelStackNamespace]; ns != "" {
		return dnsName(ns), strings.TrimPrefix(name, ns+"_")
	}
	return api.SwarmNamespace, name
}

func splitVolumeNS(source, ns string) (string, string) {
	if ns != api.SwarmNamespace && strings.HasPrefix(source, ns+"_") {
		return ns, strings.TrimPrefix(source, ns+"_")
	}
	return api.SwarmNamespace, source
}

var dnsInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// dnsName makes a valid Kubernetes object name.
func dnsName(s string) string {
	s = dnsInvalid.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	if s == "" {
		s = "unnamed"
	}
	return s
}

func stripLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	for _, k := range ownLabels {
		delete(out, k)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// k8sLabels keeps the labels that are valid Kubernetes labels and are not
// Docker stack or Traefik labels.
func k8sLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if strings.HasPrefix(k, "traefik.") || strings.HasPrefix(k, "com.docker.stack.") {
			continue
		}
		if len(validation.IsQualifiedName(k)) > 0 || len(validation.IsValidLabelValue(v)) > 0 {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func numericUser(u string) (*int64, *int64, bool) {
	if u == "" {
		return nil, nil, false
	}
	us, gs, hasG := strings.Cut(u, ":")
	uid, err := strconv.ParseInt(us, 10, 64)
	if err != nil {
		return nil, nil, false
	}
	if !hasG {
		return &uid, nil, true
	}
	gid, err := strconv.ParseInt(gs, 10, 64)
	if err != nil {
		return nil, nil, false
	}
	return &uid, &gid, true
}

func quantities(nano, mem int64) corev1.ResourceList {
	out := corev1.ResourceList{}
	if nano > 0 {
		out[corev1.ResourceCPU] = *resource.NewMilliQuantity(nano/1_000_000, resource.DecimalSI)
	}
	if mem > 0 {
		out[corev1.ResourceMemory] = *resource.NewQuantity(mem, resource.BinarySI)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nodeSelector(cons []string) map[string]string {
	out := map[string]string{}
	for _, c := range cons {
		k, v, ok := strings.Cut(c, "==")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case k == "node.hostname":
			out["kubernetes.io/hostname"] = v
		case k == "node.platform.os":
			out["kubernetes.io/os"] = v
		case k == "node.platform.arch":
			switch v {
			case "x86_64":
				v = "amd64"
			case "aarch64":
				v = "arm64"
			}
			out["kubernetes.io/arch"] = v
		case k == "node.role" && v == "manager":
			out["node-role.kubernetes.io/control-plane"] = ""
		case strings.HasPrefix(k, "node.labels."):
			out[strings.TrimPrefix(k, "node.labels.")] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func i32p(v *uint64) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v)
	return &n
}

func appendVolume(vols []corev1.Volume, v corev1.Volume) []corev1.Volume {
	for _, e := range vols {
		if e.Name == v.Name {
			return vols
		}
	}
	return append(vols, v)
}

func uniqueVol(vols []corev1.Volume, name string) string {
	name = dnsName(name)
	base, i := name, 1
	for {
		clash := false
		for _, v := range vols {
			if v.Name == name {
				clash = true
			}
		}
		if !clash {
			return name
		}
		i++
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

var azureUnsafe = regexp.MustCompile(`[^A-Za-z0-9-]`)

// azureName makes a Key Vault compatible secret name (alphanumerics and dashes).
func azureName(s string) string {
	return azureUnsafe.ReplaceAllString(s, "-")
}

func secretStore(o Options, ns string, keys []string) *api.SecretStore {
	st := &api.SecretStore{
		TypeMeta: api.TypeMeta{APIVersion: api.ESOGroupVersion, Kind: o.SecretStoreKind},
		Metadata: api.ObjectMeta{Name: o.SecretStore, Namespace: ns},
	}
	switch o.SecretStoreProvider {
	case ProviderFake:
		f := &api.FakeProvider{}
		for _, k := range keys {
			f.Data = append(f.Data, api.FakeProviderData{Key: k, Value: "REPLACE_ME"})
		}
		st.Spec.Provider.Fake = f
	case ProviderAzureKV:
		st.Spec.Provider.AzureKV = &api.AzureKVProvider{
			VaultURL: "https://REPLACE_ME.vault.azure.net",
			TenantID: "REPLACE_ME",
			AuthType: "ServicePrincipal",
			AuthSecretRef: &api.AzureKVAuth{
				ClientID:     &api.SecretKeySelector{Name: "azure-keyvault", Key: "clientId"},
				ClientSecret: &api.SecretKeySelector{Name: "azure-keyvault", Key: "clientSecret"},
			},
		}
	default:
		st.Spec.Provider.Vault = &api.VaultProvider{
			Server:  "https://vault.example.com:8200",
			Path:    "secret",
			Version: "v2",
			Auth:    api.VaultAuth{TokenSecretRef: &api.SecretKeySelector{Name: "vault-token", Key: "token"}},
		}
	}
	return st
}
