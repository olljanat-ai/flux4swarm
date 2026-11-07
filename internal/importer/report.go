// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// ReportFile is the migration readiness report, relative to the cluster
// directory. The export owns it and rewrites it on every sync, so it is a
// checklist that shrinks as the Swarm services are changed.
const ReportFile = "MIGRATION.md"

// Severities of a Finding.
const (
	// Blocker must change before the service can run on Kubernetes.
	Blocker = "blocker"
	// Check works on Kubernetes but needs a decision or a setting there.
	Check = "check"
)

// Finding is one item of the migration readiness report.
type Finding struct {
	Severity string `json:"severity"`
	Text     string `json:"text"`
}

// ServiceReport lists what stands between one Swarm service and
// Kubernetes.
type ServiceReport struct {
	// Swarm is the Swarm service name.
	Swarm string `json:"swarm"`
	// Kind, Namespace and Name identify the exported workload.
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Findings  []Finding `json:"findings,omitempty"`
}

// Blockers counts the blocker findings.
func (s ServiceReport) Blockers() int {
	n := 0
	for _, f := range s.Findings {
		if f.Severity == Blocker {
			n++
		}
	}
	return n
}

// Report is the Kubernetes migration readiness of the exported Swarm.
type Report struct {
	Services []ServiceReport `json:"services"`
	// General are findings that are not about one service.
	General []Finding `json:"general,omitempty"`
	Skipped []Skipped `json:"skipped,omitempty"`
}

// reportService is a service as exported, for the report.
type reportService struct {
	swarm, kind, ns, name string
}

// secretEnv matches environment variable names that usually hold a
// credential.
var secretEnv = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|PASSPHRASE|SECRET|TOKEN|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY|CREDENTIAL|AUTH_?KEY|CONN(ECTION)?_?STR(ING)?)`)

// urlCredentials matches a URL with a password, e.g. postgres://u:p@db/x.
var urlCredentials = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@:\s]+:[^/@\s]+@`)

// placement constraints the export maps to a nodeSelector.
var mappedConstraint = regexp.MustCompile(`^(node\.hostname|node\.platform\.os|node\.platform\.arch|node\.labels\.[^=!]+)==|^node\.role==manager$`)

// report builds the migration readiness report after the round trip.
func (e *exporter) report() {
	r := &Report{Skipped: e.res.Skipped}
	for _, s := range e.services {
		sr := ServiceReport{Swarm: s.swarm, Kind: s.kind, Namespace: s.ns, Name: s.name}
		add := func(sev, format string, args ...any) {
			sr.Findings = append(sr.Findings, Finding{Severity: sev, Text: fmt.Sprintf(format, args...)})
		}
		spec := e.original[s.swarm]
		cs := spec.TaskTemplate.ContainerSpec

		// Mounts.
		for _, m := range cs.Mounts {
			switch m.Type {
			case mount.TypeBind:
				if strings.HasSuffix(m.Source, "/docker.sock") || strings.HasSuffix(m.Source, `\docker_engine`) {
					add(Blocker, "mounts the Docker socket `%s`: there is no Docker API on Kubernetes; use the Kubernetes API (a ServiceAccount with RBAC) or drop the dependency", m.Source)
					continue
				}
				add(Blocker, "bind mount `%s` → `%s` becomes a hostPath volume, which ties the pod to files on one node; move the data to a volume, ConfigMap or Secret", m.Source, m.Target)
			case mount.TypeVolume:
				if m.VolumeOptions != nil && m.VolumeOptions.DriverConfig != nil && m.VolumeOptions.DriverConfig.Name != "" && m.VolumeOptions.DriverConfig.Name != "local" {
					add(Check, "volume `%s` uses Docker volume driver `%s`: pick a Kubernetes StorageClass for its PersistentVolumeClaim", m.Source, m.VolumeOptions.DriverConfig.Name)
				} else if m.VolumeOptions != nil && m.VolumeOptions.DriverConfig != nil && len(m.VolumeOptions.DriverConfig.Options) > 0 {
					add(Check, "volume `%s` uses local driver options (e.g. an NFS or CIFS mount): pick a Kubernetes StorageClass or PersistentVolume for it", m.Source)
				}
			case mount.TypeTmpfs:
			default:
				add(Blocker, "mount of type %s at `%s` has no Kubernetes equivalent", m.Type, m.Target)
			}
		}

		// Published ports.
		if ep := spec.EndpointSpec; ep != nil {
			for _, p := range ep.Ports {
				if p.PublishMode == swarm.PortConfigPublishModeHost {
					add(Blocker, "port %d/%s is published in host mode (on the node's own address): clients that connect to a node IP need a LoadBalancer or NodePort address on Kubernetes", p.PublishedPort, protocol(p.Protocol))
				} else {
					add(Check, "port %d/%s is published on the routing mesh: it becomes a LoadBalancer Service, which needs a load balancer on the cluster (cloud or MetalLB)", p.PublishedPort, protocol(p.Protocol))
				}
			}
			if ep.Mode == swarm.ResolutionModeDNSRR {
				add(Check, "uses DNS round robin endpoint mode: a headless Service is the Kubernetes equivalent")
			}
		}

		// Placement.
		if p := spec.TaskTemplate.Placement; p != nil {
			for _, c := range p.Constraints {
				c = strings.ReplaceAll(c, " ", "")
				switch {
				case !mappedConstraint.MatchString(c):
					add(Blocker, "placement constraint `%s` has no nodeSelector equivalent: use node affinity or taints and tolerations", c)
				case strings.HasPrefix(c, "node.hostname=="):
					add(Check, "pinned to node `%s` (constraint `%s`): usually for local data, which should move to a volume that any node can mount", strings.TrimPrefix(c, "node.hostname=="), c)
				case strings.HasPrefix(c, "node.labels."):
					k, v, _ := strings.Cut(strings.TrimPrefix(c, "node.labels."), "==")
					add(Check, "needs Kubernetes nodes labelled `%s=%s` (constraint `%s`)", k, v, c)
				case c == "node.role==manager":
					add(Check, "runs on managers only: on Kubernetes that is the control plane, which usually does not run workloads; check whether it is still needed")
				}
			}
			if len(p.Preferences) > 0 {
				add(Check, "has placement preferences (spread): use topologySpreadConstraints")
			}
			if p.MaxReplicas > 0 {
				add(Check, "limits replicas per node to %d: use pod anti-affinity or topologySpreadConstraints", p.MaxReplicas)
			}
		}

		// Secrets in plain text environment variables. Values are never
		// written to the report.
		var envNames []string
		for _, kv := range cs.Env {
			k, v, _ := strings.Cut(kv, "=")
			if v == "" || strings.HasSuffix(strings.ToUpper(k), "_FILE") {
				continue
			}
			if secretEnv.MatchString(k) || urlCredentials.MatchString(v) {
				envNames = append(envNames, "`"+k+"`")
			}
		}
		if len(envNames) > 0 {
			add(Blocker, "environment %s look like plain text credentials, which are now committed to Git: move them to a Docker secret (an ExternalSecret on Kubernetes)", strings.Join(envNames, ", "))
		}

		if u := cs.User; u != "" {
			if _, _, ok := numericUser(u); !ok {
				add(Check, "runs as user `%s`: Kubernetes needs a numeric runAsUser", u)
			}
		}

		if ld := spec.TaskTemplate.LogDriver; ld != nil && ld.Name != "" && ld.Name != "json-file" && ld.Name != "local" {
			add(Check, "logs with the Docker `%s` log driver: on Kubernetes containers log to stdout and a cluster log collector (e.g. Fluent Bit) ships them", ld.Name)
		}

		// Whatever the manifests could not express.
		if meta := e.workload[s.swarm]; meta != nil && meta.Annotations[api.AnnotationServiceSpec] != "" {
			if paths := patchPaths(meta.Annotations[api.AnnotationServiceSpec]); len(paths) > 0 {
				add(Check, "Swarm-only settings are kept in the `%s` annotation and are ignored on Kubernetes: %s", api.AnnotationServiceSpec, strings.Join(paths, ", "))
			}
		}
		if meta := e.workload[s.swarm]; meta != nil && meta.Annotations[api.AnnotationNetworks] != "" {
			add(Check, "attached to networks %s: Kubernetes has one flat network; use NetworkPolicies to keep it isolated", quoteCode(strings.Split(meta.Annotations[api.AnnotationNetworks], ",")))
		}

		// Export notes about this service (Traefik labels, volumes, ...).
		prefix := "service " + s.swarm + ": "
		for _, n := range e.notes {
			if strings.HasPrefix(n, prefix) {
				add(Check, "%s", strings.TrimPrefix(n, prefix))
			}
		}
		r.Services = append(r.Services, sr)
	}
	for _, n := range e.notes {
		if !strings.HasPrefix(n, "service ") {
			r.General = append(r.General, Finding{Severity: Check, Text: n})
		}
	}
	sort.SliceStable(r.Services, func(i, j int) bool {
		a, b := r.Services[i], r.Services[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	e.res.Report = r
	e.res.Files[e.o.Path+"/"+ReportFile] = r.Markdown(e.o.Path)
	e.res.Clean = append(e.res.Clean, e.o.Path+"/"+ReportFile)
}

func protocol(p swarm.PortConfigProtocol) string {
	if p == "" {
		return "tcp"
	}
	return string(p)
}

func quoteCode(l []string) string {
	q := make([]string, len(l))
	for i, s := range l {
		q[i] = "`" + s + "`"
	}
	return strings.Join(q, ", ")
}

// reportedElsewhere are service-spec annotation paths the report covers
// with its own findings, or that have a Kubernetes counterpart already
// (rollout and restart tuning, labels that are not valid Kubernetes
// labels), so listing them would only add noise.
var reportedElsewhere = []string{
	"EndpointSpec", "Mode", "UpdateConfig", "RollbackConfig", "Labels", "TaskTemplate.Placement",
	"TaskTemplate.Networks", "TaskTemplate.RestartPolicy", "TaskTemplate.ContainerSpec.Mounts",
	"TaskTemplate.ContainerSpec.Labels", "TaskTemplate.ContainerSpec.User", "TaskTemplate.LogDriver",
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// patchPaths returns the dotted paths a JSON merge patch sets, sorted,
// without the ones in reportedElsewhere.
func patchPaths(patch string) []string {
	var m map[string]any
	if json.Unmarshal([]byte(patch), &m) != nil {
		return nil
	}
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			p := prefix + k
			if contains(reportedElsewhere, p) {
				continue
			}
			if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
				walk(p+".", sub)
				continue
			}
			out = append(out, "`"+p+"`")
		}
	}
	walk("", m)
	sort.Strings(out)
	return out
}

// Markdown renders the report. It has no timestamps, so an unchanged
// Swarm gives an unchanged file and no commit.
func (r *Report) Markdown(root string) []byte {
	var b strings.Builder
	ready, blockers, checks := 0, 0, 0
	for _, s := range r.Services {
		n := s.Blockers()
		if n == 0 {
			ready++
		}
		blockers += n
		checks += len(s.Findings) - n
	}
	fmt.Fprintf(&b, "<!-- Generated by flux4swarm import; rewritten on every sync, do not edit. -->\n")
	fmt.Fprintf(&b, "# Kubernetes migration readiness: %s\n\n", root)
	b.WriteString("What stands between each Swarm service and Kubernetes. **Blockers** must change\n" +
		"on Swarm (or be solved on Kubernetes) before the service can move; **checks** work\n" +
		"after the move but need a decision or a setting on the cluster. Fix them on Swarm\n" +
		"and the list shrinks with the next sync.\n\n")
	fmt.Fprintf(&b, "**%d of %d services have no blockers.** %d blocker%s, %d check%s in total.\n\n",
		ready, len(r.Services), blockers, plural(blockers), checks, plural(checks))
	if len(r.Services) > 0 {
		b.WriteString("| Service | Swarm name | Blockers | Checks |\n|---|---|---|---|\n")
		for _, s := range r.Services {
			n := s.Blockers()
			fmt.Fprintf(&b, "| %s %s/%s | `%s` | %d | %d |\n", s.Kind, s.Namespace, s.Name, s.Swarm, n, len(s.Findings)-n)
		}
		b.WriteString("\n")
	}
	for _, s := range r.Services {
		if len(s.Findings) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s %s/%s\n\n", s.Kind, s.Namespace, s.Name)
		for _, sev := range []string{Blocker, Check} {
			for _, f := range s.Findings {
				if f.Severity == sev {
					fmt.Fprintf(&b, "- [ ] **%s:** %s\n", sev, oneLine(f.Text))
				}
			}
		}
		b.WriteString("\n")
	}
	if len(r.General) > 0 {
		b.WriteString("## Cluster\n\n")
		for _, f := range r.General {
			fmt.Fprintf(&b, "- [ ] %s\n", oneLine(f.Text))
		}
		b.WriteString("\n")
	}
	if len(r.Skipped) > 0 {
		b.WriteString("## Not exported\n\n")
		for _, s := range r.Skipped {
			fmt.Fprintf(&b, "- %s `%s`: %s\n", s.Kind, s.Name, s.Reason)
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
