//go:build e2e

package e2e

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// TestVolumeRoundTrip: a Swarm service with a named volume and a bind mount
// is imported as a PersistentVolumeClaim + hostPath volume, reconciling the
// import back is a no-op on Swarm, and stock Flux2 applies the export.
func TestVolumeRoundTrip(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("vol")
	cleanupStack(t, ns)
	svc := createStackService(t, ns, "db", "--replicas", "1",
		"--mount", "type=volume,source="+ns+"_data,target=/data",
		"--mount", "type=bind,source=/tmp,target=/hostdata,readonly")
	waitServiceRunning(t, svc, 1)

	importToGit(t, repo, importFlags...)
	repo.pull()

	pvc := repo.mustRead(importedPVC(ns, "data"))
	for _, want := range []string{"kind: PersistentVolumeClaim", "name: data", "namespace: " + ns, "ReadWriteOnce", "storage: 1Gi"} {
		if !strings.Contains(pvc, want) {
			t.Errorf("%s lacks %q:\n%s", importedPVC(ns, "data"), want, pvc)
		}
	}
	if strings.Contains(pvc, "storageClassName") {
		t.Errorf("%s must not set storageClassName:\n%s", importedPVC(ns, "data"), pvc)
	}
	dep := repo.mustRead(importedDeployment(ns, "db"))
	for _, want := range []string{"claimName: data", "hostPath:", "path: /tmp", "mountPath: /data", "mountPath: /hostdata"} {
		if !strings.Contains(dep, want) {
			t.Errorf("%s lacks %q:\n%s", importedDeployment(ns, "db"), want, dep)
		}
	}

	assertReconcileIsNoop(t, repo, svc)
	after := mustService(t, svc)
	mounts := map[string]string{}
	for _, m := range after.Spec.TaskTemplate.ContainerSpec.Mounts {
		mounts[m.Target] = m.Type + ":" + m.Source
	}
	if mounts["/data"] != "volume:"+ns+"_data" || mounts["/hostdata"] != "bind:/tmp" {
		t.Errorf("swarm mounts after reconcile: %v", mounts)
	}

	t.Run("flux2", func(t *testing.T) {
		k := requireK8s(t)
		k.validateApps(t, repo)
		s := k.sync(t, repo, fluxSyncOpts{Wait: false})
		s.reconcile(t)
		var c struct {
			Spec struct {
				AccessModes []string
				Resources   struct{ Requests map[string]string }
			}
		}
		if !k.getJSON(t, ns, "persistentvolumeclaim", "data", &c) {
			t.Fatalf("k8s: PVC %s/data not created", ns)
		}
		if strings.Join(c.Spec.AccessModes, ",") != "ReadWriteOnce" || c.Spec.Resources.Requests["storage"] != "1Gi" {
			t.Errorf("k8s: PVC %s/data spec = %+v", ns, c.Spec)
		}
		d := k.mustDeployment(t, ns, "db")
		vols, _ := json.Marshal(d.Spec.Template.Spec.Volumes)
		if !strings.Contains(string(vols), `"claimName":"data"`) || !strings.Contains(string(vols), `"hostPath"`) {
			t.Errorf("k8s: deployment %s/db volumes = %s", ns, vols)
		}
	})
}

// TestTraefikLabelsToGateway: Traefik Swarm provider labels are imported as a
// Gateway API HTTPRoute (+ Service, GatewayClass/Gateway), reconciling back
// keeps equivalent Traefik labels on Swarm, and stock Flux2 applies the
// export (Gateway API CRDs only, no gateway controller).
func TestTraefikLabelsToGateway(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("trf")
	cleanupStack(t, ns)
	const host, rule = "web.example.test", "Host(`web.example.test`) && PathPrefix(`/`)"
	svc := createStackService(t, ns, "web", "--replicas", "1",
		"--label", "traefik.enable=true",
		"--label", "traefik.http.routers.web.rule="+rule,
		"--label", "traefik.http.routers.web.entrypoints=web",
		"--label", "traefik.http.services.web.loadbalancer.server.port=80")
	waitServiceRunning(t, svc, 1)

	importToGit(t, repo, importFlags...)
	repo.pull()

	route := repo.mustRead(importedHTTPRoute(ns, "web"))
	for _, want := range []string{"gateway.networking.k8s.io/v1", "kind: HTTPRoute", "name: web", "namespace: " + ns,
		host, "PathPrefix", "sectionName: web", "name: traefik", "port: 80"} {
		if !strings.Contains(route, want) {
			t.Errorf("%s lacks %q:\n%s", importedHTTPRoute(ns, "web"), want, route)
		}
	}
	gw := repo.mustRead(importedGateway())
	for _, want := range []string{"kind: GatewayClass", "traefik.io/gateway-controller", "kind: Gateway", "namespace: traefik", "port: 80", "protocol: HTTP"} {
		if !strings.Contains(gw, want) {
			t.Errorf("%s lacks %q:\n%s", importedGateway(), want, gw)
		}
	}
	if !anyFileContains(repo, appDir(ns)+"/service-*.yaml", "kind: Service", "port: 80") {
		t.Errorf("no %s/service-*.yaml with a Service on port 80; tree:\n%s", appDir(ns), repo.git("ls-files"))
	}

	reconcile(t, repo)
	labels := mustService(t, svc).Spec.Labels
	norm := regexp.MustCompile(`\s+`)
	want := map[string]string{
		"traefik.enable":                                     "true",
		"traefik.http.routers.web.rule":                      norm.ReplaceAllString(rule, ""),
		"traefik.http.routers.web.entrypoints":               "web",
		"traefik.http.services.web.loadbalancer.server.port": "80",
	}
	for k, v := range want {
		if got := norm.ReplaceAllString(labels[k], ""); got != v {
			t.Errorf("swarm label %s = %q, want %q (labels: %v)", k, labels[k], v, labels)
		}
	}

	t.Run("flux2", func(t *testing.T) {
		k := requireK8s(t)
		k.validateApps(t, repo)
		s := k.sync(t, repo, fluxSyncOpts{Wait: false})
		s.reconcile(t)
		var r struct {
			Spec struct {
				Hostnames  []string
				ParentRefs []struct{ Name, Namespace, SectionName string }
				Rules      []struct {
					Matches []struct {
						Path struct{ Type, Value string }
					}
					BackendRefs []struct {
						Name string
						Port int
					}
				}
			}
		}
		if !k.getJSON(t, ns, "httproute", "web", &r) {
			t.Fatalf("k8s: HTTPRoute %s/web not created", ns)
		}
		sp := r.Spec
		if len(sp.Hostnames) != 1 || sp.Hostnames[0] != host {
			t.Errorf("k8s: hostnames = %v", sp.Hostnames)
		}
		if len(sp.ParentRefs) != 1 || sp.ParentRefs[0].Name != "traefik" || sp.ParentRefs[0].SectionName != "web" {
			t.Errorf("k8s: parentRefs = %+v", sp.ParentRefs)
		}
		if len(sp.Rules) != 1 || len(sp.Rules[0].Matches) != 1 || sp.Rules[0].Matches[0].Path.Type != "PathPrefix" ||
			sp.Rules[0].Matches[0].Path.Value != "/" || len(sp.Rules[0].BackendRefs) != 1 || sp.Rules[0].BackendRefs[0].Port != 80 {
			t.Fatalf("k8s: rules = %+v", sp.Rules)
		}
		if be := sp.Rules[0].BackendRefs[0].Name; !k.exists(t, ns, "service", be) {
			t.Errorf("k8s: backend Service %s/%s not created", ns, be)
		}
		if !k.exists(t, "traefik", "gateway", "traefik") || !k.exists(t, "", "gatewayclass", "traefik") {
			t.Errorf("k8s: Gateway traefik/traefik or GatewayClass traefik not created")
		}
	})
}
