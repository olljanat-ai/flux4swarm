// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"os"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

type fakeKube struct {
	objs map[string][]string // Kind -> YAML documents
}

var fakeResources = []KubeResource{
	{Version: "v1", Resource: "namespaces", Kind: "Namespace"},
	{Version: "v1", Resource: "services", Kind: "Service", Namespaced: true},
	{Version: "v1", Resource: "secrets", Kind: "Secret", Namespaced: true},
	{Version: "v1", Resource: "configmaps", Kind: "ConfigMap", Namespaced: true},
	{Version: "v1", Resource: "persistentvolumeclaims", Kind: "PersistentVolumeClaim", Namespaced: true},
	{Version: "v1", Resource: "events", Kind: "Event", Namespaced: true},
	{Version: "v1", Resource: "nodes", Kind: "Node"},
	{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Namespaced: true},
	{Group: "apps", Version: "v1", Resource: "replicasets", Kind: "ReplicaSet", Namespaced: true},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles", Kind: "ClusterRole"},
}

func (f *fakeKube) Resources(context.Context) ([]KubeResource, error) { return fakeResources, nil }

func (f *fakeKube) List(_ context.Context, r KubeResource, ns string) ([]map[string]any, error) {
	var out []map[string]any
	for _, doc := range f.objs[r.Kind] {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			return nil, err
		}
		if ns != "" && str(m, "metadata", "namespace") != ns {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func helmSecret(t *testing.T) string {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(`{"name":"cache","chart":{"metadata":{"name":"redis","version":"17.3.2"}}}`))
	_ = w.Close()
	inner := base64.StdEncoding.EncodeToString(gz.Bytes())
	outer := base64.StdEncoding.EncodeToString([]byte(inner))
	return `
metadata: {name: sh.helm.release.v1.cache.v1, namespace: shop, labels: {owner: helm, name: cache}}
type: helm.sh/release.v1
data: {release: ` + outer + `}`
}

func testCluster(t *testing.T) *fakeKube {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return &fakeKube{objs: map[string][]string{
		"Namespace": {
			`metadata: {name: shop, uid: x, labels: {kubernetes.io/metadata.name: shop, pod-security.kubernetes.io/enforce: baseline}}
spec: {finalizers: [kubernetes]}
status: {phase: Active}`,
			`metadata: {name: kube-system}`,
		},
		"Deployment": {`
metadata:
  name: api
  namespace: shop
  uid: 1234
  resourceVersion: "99"
  generation: 3
  creationTimestamp: "2026-01-01T00:00:00Z"
  managedFields: [{manager: kubectl}]
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "{}"
    deployment.kubernetes.io/revision: "3"
    team: shop
spec:
  replicas: 2
  selector: {matchLabels: {app: api}}
  template:
    metadata:
      labels: {app: api}
      annotations: {kubectl.kubernetes.io/restartedAt: "2026-01-02T00:00:00Z"}
    spec:
      containers: [{name: api, image: "ghcr.io/acme/api:1.4"}]
      volumes: [{name: data, persistentVolumeClaim: {claimName: data}}]
status: {readyReplicas: 2}`,
			`
metadata: {name: worker, namespace: shop, annotations: {flux4swarm.io/stopped-replicas: "3"}}
spec:
  replicas: 0
  selector: {matchLabels: {app: worker}}
  template:
    metadata: {labels: {app: worker}}
    spec: {containers: [{name: w, image: "ghcr.io/acme/worker:2"}]}`,
			`
metadata: {name: synced, namespace: shop, labels: {kustomize.toolkit.fluxcd.io/name: apps}}
spec: {selector: {matchLabels: {app: s}}, template: {metadata: {labels: {app: s}}, spec: {containers: [{name: s, image: s}]}}}`,
			`
metadata: {name: coredns, namespace: kube-system}
spec: {selector: {matchLabels: {app: d}}, template: {metadata: {labels: {app: d}}, spec: {containers: [{name: d, image: d}]}}}`,
		},
		"ReplicaSet": {`
metadata: {name: api-5d9, namespace: shop, ownerReferences: [{kind: Deployment, name: api}]}
spec: {selector: {matchLabels: {app: api}}, template: {metadata: {labels: {app: api}}, spec: {containers: [{name: api, image: x}]}}}`},
		"Service": {`
metadata: {name: api, namespace: shop}
spec:
  type: LoadBalancer
  clusterIP: 10.43.0.10
  clusterIPs: [10.43.0.10]
  ipFamilies: [IPv4]
  ipFamilyPolicy: SingleStack
  healthCheckNodePort: 31000
  selector: {app: api}
  ports: [{name: http, port: 80, targetPort: 8080, nodePort: 30080, protocol: TCP}]
status: {loadBalancer: {ingress: [{ip: 1.2.3.4}]}}`,
			`metadata: {name: kubernetes, namespace: default}
spec: {ports: [{port: 443}]}`},
		"PersistentVolumeClaim": {`
metadata:
  name: data
  namespace: shop
  finalizers: [kubernetes.io/pvc-protection]
  annotations: {pv.kubernetes.io/bind-completed: "yes", volume.kubernetes.io/storage-provisioner: rancher.io/local-path}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: local-path
  volumeName: pvc-1234
  resources: {requests: {storage: 5Gi}}
status: {phase: Bound}`},
		"ConfigMap": {
			`metadata: {name: kube-root-ca.crt, namespace: shop}
data: {ca.crt: x}`,
			`metadata: {name: app, namespace: shop}
data: {app.conf: "port: 8080\n"}`,
		},
		"Secret": {
			`metadata: {name: db, namespace: shop, labels: {app: api}}
type: Opaque
data: {password: ` + b64("s3cr3t") + `, tls.key: ` + base64.StdEncoding.EncodeToString([]byte{0xff, 0x00, 0x01}) + `}`,
			`metadata: {name: web-tls, namespace: shop}
type: kubernetes.io/tls
data: {tls.crt: ` + b64("CERT") + `, tls.key: ` + b64("KEY") + `}`,
			`metadata: {name: cm-issued, namespace: shop, annotations: {cert-manager.io/certificate-name: web}}
type: kubernetes.io/tls
data: {tls.crt: ` + b64("C") + `}`,
			`metadata: {name: from-eso, namespace: shop, ownerReferences: [{kind: ExternalSecret, name: x}]}
data: {a: ` + b64("b") + `}`,
			helmSecret(t),
		},
		"Event": {`metadata: {name: e1, namespace: shop}`},
		"Node":  {`metadata: {name: n1}`},
		"ClusterRole": {
			`metadata: {name: "system:node"}`,
			`metadata: {name: shop-reader}
rules: [{apiGroups: [""], resources: [pods], verbs: [get]}]`,
		},
	}}
}

func exportKube(t *testing.T, o Options, ko KubeOptions) *Result {
	t.Helper()
	res, err := ExportKubernetes(context.Background(), testCluster(t), o, ko)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func file(t *testing.T, res *Result, name string) string {
	t.Helper()
	b, ok := res.Files[name]
	if !ok {
		var names []string
		for f := range res.Files {
			names = append(names, f)
		}
		t.Fatalf("%s not exported; files: %s", name, strings.Join(names, "\n"))
	}
	return string(b)
}

func TestExportKubernetes(t *testing.T) {
	res := exportKube(t, Options{Cluster: "prod", Bootstrap: true}, KubeOptions{SecretValues: true})
	root := "clusters/prod/apps/shop/"

	d := file(t, res, root+"deployment-api.yaml")
	for _, bad := range []string{"uid", "resourceVersion", "managedFields", "last-applied", "revision", "restartedAt", "status", "creationTimestamp", "generation"} {
		if strings.Contains(d, bad) {
			t.Errorf("deployment keeps %q:\n%s", bad, d)
		}
	}
	for _, want := range []string{"team: shop", "replicas: 2", "image: ghcr.io/acme/api:1.4", "claimName: data"} {
		if !strings.Contains(d, want) {
			t.Errorf("deployment lacks %q:\n%s", want, d)
		}
	}
	if w := file(t, res, root+"deployment-worker.yaml"); !strings.Contains(w, "replicas: 3") || strings.Contains(w, api.AnnotationStoppedReplicas) {
		t.Errorf("stopped replicas not restored:\n%s", w)
	}

	svc := file(t, res, root+"service-api.yaml")
	for _, bad := range []string{"10.43.0.10", "nodePort", "healthCheckNodePort", "ipFamilies", "1.2.3.4"} {
		if strings.Contains(svc, bad) {
			t.Errorf("service keeps %q:\n%s", bad, svc)
		}
	}
	pvc := file(t, res, root+"persistentvolumeclaim-data.yaml")
	for _, bad := range []string{"volumeName", "pvc-protection", "bind-completed", "storage-provisioner"} {
		if strings.Contains(pvc, bad) {
			t.Errorf("claim keeps %q:\n%s", bad, pvc)
		}
	}
	if !strings.Contains(pvc, "storage: 5Gi") || !strings.Contains(pvc, "storageClassName: local-path") {
		t.Errorf("claim lost its spec:\n%s", pvc)
	}
	ns := file(t, res, root+"namespace.yaml")
	if !strings.Contains(ns, "pod-security.kubernetes.io/enforce: baseline") || strings.Contains(ns, "finalizers") || strings.Contains(ns, "metadata.name") {
		t.Errorf("namespace:\n%s", ns)
	}
	file(t, res, root+"configmap-app.yaml")
	file(t, res, root+"secretstore.yaml")
	file(t, res, "clusters/prod/flux-system/gotk-sync.yaml")

	es := file(t, res, root+"externalsecret-db.yaml")
	if strings.Contains(es, "s3cr3t") || !strings.Contains(es, "key: shop-db-password") || !strings.Contains(es, "decodingStrategy: Base64") || !strings.Contains(es, "app: api") {
		t.Errorf("ExternalSecret:\n%s", es)
	}
	if tls := file(t, res, root+"externalsecret-web-tls.yaml"); !strings.Contains(tls, "type: kubernetes.io/tls") || !strings.Contains(tls, "secretKey: tls.crt") {
		t.Errorf("TLS ExternalSecret:\n%s", tls)
	}
	if res.SecretValues["shop-db-password"] != "s3cr3t" || res.SecretValues["shop-db-tls-key"] != base64.StdEncoding.EncodeToString([]byte{0xff, 0, 1}) {
		t.Errorf("secret values: %v", res.SecretValues)
	}
	for f, b := range res.Files {
		if bytes.Contains(b, []byte("s3cr3t")) {
			t.Errorf("%s holds a secret value", f)
		}
	}

	for f := range res.Files {
		for _, bad := range []string{"replicaset-", "event-", "node-", "kube-root-ca", "deployment-synced", "deployment-coredns", "from-eso", "cm-issued", "sh.helm", "service-kubernetes", "clusterrole"} {
			if strings.Contains(f, bad) {
				t.Errorf("exported %s", f)
			}
		}
	}
	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.Kind+"/"+s.Name] = s.Reason
	}
	for k, want := range map[string]string{
		"Deployment/shop/synced": SkippedFlux, "Namespace/kube-system": SkippedSystemNamespace,
		"Secret/shop/cm-issued": SkippedCertManager, "Secret/shop/sh.helm.release.v1.cache.v1": SkippedHelmRelease,
	} {
		if skipped[k] != want {
			t.Errorf("skipped %s = %q, want %q (all: %v)", k, skipped[k], want, skipped)
		}
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "Helm release shop/cache (chart redis 17.3.2)") {
		t.Errorf("notes lack the Helm release: %v", res.Notes)
	}

	dir := t.TempDir()
	if err := res.WriteTo(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := Validate(dir, res.Root); err != nil {
		t.Fatal(err)
	}
	// Deterministic, so --watch only commits real changes.
	if again := exportKube(t, Options{Cluster: "prod", Bootstrap: true}, KubeOptions{SecretValues: true}); again.Hash() != res.Hash() {
		t.Error("export is not deterministic")
	}
}

func TestExportKubernetesOptions(t *testing.T) {
	res := exportKube(t, Options{Cluster: "prod", SecretStoreProvider: ProviderAzureKV, SecretKeyPrefix: "k8s."}, KubeOptions{
		ClusterScoped: true, StorageClasses: map[string]string{"local-path": "managed-csi"},
		Exclude: regexp.MustCompile(`^shop/ConfigMap/`), Namespaces: []string{"shop", "kube-system"},
	})
	if f := file(t, res, "clusters/prod/apps/shop/persistentvolumeclaim-data.yaml"); !strings.Contains(f, "storageClassName: managed-csi") {
		t.Errorf("storage class not renamed:\n%s", f)
	}
	file(t, res, "clusters/prod/apps/"+ClusterDir+"/clusterrole-shop-reader.yaml")
	file(t, res, "clusters/prod/apps/kube-system/deployment-coredns.yaml")
	if _, ok := res.Files["clusters/prod/apps/shop/configmap-app.yaml"]; ok {
		t.Error("--exclude ignored")
	}
	if es := file(t, res, "clusters/prod/apps/shop/externalsecret-db.yaml"); !strings.Contains(es, "key: k8s-shop-db-password") {
		t.Errorf("Azure key name:\n%s", es)
	}
}

func TestExportKubernetesVolSync(t *testing.T) {
	res := exportKube(t, Options{Cluster: "dr", VolSync: true, VolSyncSchedule: "0 * * * *"}, KubeOptions{})
	root := "clusters/dr/apps/shop/"
	rd := file(t, res, root+"replicationdestination-data.yaml")
	if !strings.Contains(rd, "destinationPVC: data") || !strings.Contains(rd, "schedule: 0 * * * *") {
		t.Errorf("ReplicationDestination:\n%s", rd)
	}
	file(t, res, root+"externalsecret-data-volsync-restic.yaml")
	d := file(t, res, root+"deployment-api.yaml")
	if !strings.Contains(d, "replicas: 0") || !strings.Contains(d, api.AnnotationHeld+`: "2"`) {
		t.Errorf("workload with a claim not held:\n%s", d)
	}
	if w := file(t, res, root+"deployment-worker.yaml"); !strings.Contains(w, "replicas: 3") {
		t.Errorf("workload without a claim held:\n%s", w)
	}
	if p := file(t, res, root+"persistentvolumeclaim-data.yaml"); !strings.Contains(p, "kustomize.toolkit.fluxcd.io/prune: disabled") {
		t.Errorf("claim can be pruned:\n%s", p)
	}

	rf := exportKube(t, Options{Cluster: "dr", VolSyncRestoreFirst: true, Bootstrap: true}, KubeOptions{})
	file(t, rf, "clusters/dr/data/shop/persistentvolumeclaim-data.yaml")
	file(t, rf, "clusters/dr/data/shop/replicationdestination-data.yaml")
	file(t, rf, "clusters/dr/"+LayersFile)
	if d := file(t, rf, "clusters/dr/apps/shop/deployment-api.yaml"); !strings.Contains(d, "replicas: 2") {
		t.Errorf("restore first holds the workload:\n%s", d)
	}
	dir := t.TempDir()
	if err := rf.WriteTo(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := Validate(dir, rf.Root); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(dir)
}
