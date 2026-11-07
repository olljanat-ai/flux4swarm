# Monitoring

The controller serves Prometheus metrics on `/metrics` of its status port
(`:9797`). The names and labels are the ones the Flux controllers export,
plus the `gotk_resource_info` series that the
[flux2-monitoring-example](https://github.com/fluxcd/flux2-monitoring-example)
kube-state-metrics configuration produces on Kubernetes. One Grafana dashboard
and one set of alerts therefore cover Flux on Kubernetes and flux4swarm on
Swarm.

![Flux GitOps dashboard](images/grafana-flux-gitops.png)

## Metrics

| Metric | Type | Labels | Flux source |
|--------|------|--------|-------------|
| `gotk_resource_info` | gauge, always 1 | `customresource_group`, `customresource_kind`, `customresource_version`, `exported_namespace`, `name`, `ready`, `reason`, `suspended`, `revision`, `url` (GitRepository), `source_name` (Kustomization) | kube-state-metrics custom resource config of flux2-monitoring-example |
| `gotk_reconcile_duration_seconds` | histogram | `kind`, `name`, `namespace` | `fluxcd/pkg/runtime/metrics`, same buckets |
| `gotk_reconcile_condition` | gauge | `kind`, `name`, `namespace`, `type="Ready"`, `status` (`True`, `False`, `Unknown`) | `fluxcd/pkg/runtime/metrics` (deprecated in Flux 2.1, kept for older dashboards) |
| `gotk_suspend_status` | gauge | `kind`, `name`, `namespace` | `fluxcd/pkg/runtime/metrics` (deprecated in Flux 2.1) |
| `controller_runtime_reconcile_total` | counter | `controller` (`gitrepository`, `kustomization`), `result` (`success`, `error`, `requeue_after`) | controller-runtime |
| `controller_runtime_reconcile_errors_total` | counter | `controller` | controller-runtime |
| `controller_runtime_reconcile_time_seconds` | histogram | `controller` | controller-runtime |
| `controller_runtime_max_concurrent_reconciles` | gauge | `controller` | controller-runtime |
| `go_*`, `process_*` | | | Prometheus Go client |

Results follow what the Flux reconcilers return to controller-runtime: a
successful reconcile or one waiting for `dependsOn` requeues after the
interval (`requeue_after`), a failure is an `error`, and a suspended object
returns `success`. Suspended objects record no `gotk_reconcile_duration_seconds`.

On Kubernetes, Prometheus renames the `namespace` label of controller metrics
to `exported_namespace` because the scrape adds the pod's namespace. On Swarm
the label stays `namespace`. The dashboard below handles both.

## Grafana dashboard

[deploy/monitoring/grafana/flux-gitops.json](../deploy/monitoring/grafana/flux-gitops.json)
shows ready, failing and suspended resources, a table of every GitRepository
and Kustomization with its revision and reason, reconcile duration, reconcile
results, Ready status over time and the controller's CPU and memory. It uses
only metrics both platforms have, so point it at a Prometheus that scrapes
Flux on Kubernetes (with the kube-state-metrics config of
flux2-monitoring-example), flux4swarm, or both, and filter with the
namespace and kind variables.

## Running it on Swarm

[deploy/monitoring/monitoring-stack.yaml](../deploy/monitoring/monitoring-stack.yaml)
runs Prometheus and Grafana with the dashboard provisioned. It expects the
controller of [deploy/flux4swarm-stack.yaml](../deploy/flux4swarm-stack.yaml)
in stack `flux-system` and scrapes it over that stack's network:

```sh
docker config create prometheus.yml deploy/monitoring/prometheus.yml
docker config create grafana-datasource deploy/monitoring/grafana/provisioning/datasources/prometheus.yaml
docker config create grafana-dashboards deploy/monitoring/grafana/provisioning/dashboards/flux.yaml
docker config create grafana-flux-gitops deploy/monitoring/grafana/flux-gitops.json
docker stack deploy -c deploy/monitoring/monitoring-stack.yaml monitoring
```

To use an existing Prometheus instead, add a scrape job for port `9797` of the
controller, for example with `dns_sd_configs` on `tasks.controller` as in
[deploy/monitoring/prometheus.yml](../deploy/monitoring/prometheus.yml), and
import the dashboard JSON in Grafana.

## Alerts

Flux's alerting examples work unchanged, for example:

```yaml
- alert: FluxReconciliationFailure
  expr: max by (customresource_kind, exported_namespace, name) (gotk_resource_info{ready="False"}) == 1
  for: 10m
```
