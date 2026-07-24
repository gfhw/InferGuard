# InferGuard

AI inference deployment, real-time monitoring, and auto-remediation operator for Kubernetes. Deploy vLLM/TGI/SGLang with Helm, let InferGuard keep them running.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/dl/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28+-326CE5?style=flat&logo=kubernetes)](https://kubernetes.io/)
[![Helm](https://img.shields.io/badge/Helm-3.14+-0F1689?style=flat&logo=helm)](https://helm.sh/)

---

## What problem does this solve?

vLLM, TGI, and other inference engines excel at fast token generation but lack operational capabilities. You run helm install vllm, it deploys, and you're on your own for:

- **Runtime health**: Is the model really serving? What's the P99 latency right now?
- **Auto-remediation**: GPU OOM? Latency spike? Token quality degraded? Who rolls back?
- **Prometheus**: You need DCGM exporter + ServiceMonitor + Grafana dashboards ? per model.
- **Canary releases**: 10% traffic ? new model version ? auto-promote or auto-rollback.

**InferGuard wraps any Helm-based inference engine and adds the operational layer Kubernetes promised but AI workloads never got.**

---

## Core capabilities

### Helm lifecycle (declarative)
| Operation | How |
|-----------|-----|
| Deploy Model | Create CR ? Operator runs helm install vllm --values ... |
| Upgrade | Change alues or chart.version ? auto helm upgrade |
| Rollback | Set 	argetRevision ? helm rollback, syncs CR spec automatically |
| Uninstall | Delete CR ? Finalizer triggers helm uninstall to release GPU resources |
| Local Chart | chart.localPath loads .tgz or directory directly |

### AI inference monitoring
- **Active scrape**: Prometheus Collector fetches /metrics from each inference pod in real time ? **NOT from Informer cache**.
- **vLLM-native metrics**: latency, TTFT, token throughput, requests running/waiting, GPU KV-cache usage.
- **Pod-level metrics**: Phase, Ready, Restart ? from Informer (eventually consistent, supplemented by active scrape).
- **Three-channel output**: HTTP Webhook Push + Prometheus Pull + CR Status write-back.

### Smart auto-remediation
- **Policy engine**: Declarative conditions (PodRestart > 5 ? rollback, InferenceLatency > 5s ? rollback).
- **Error classification**: Distinguishes chart errors (no retry) from network glitches (exponential backoff, max 10 retries).
- **One-rollback guard**: Auto-rollback fires once per incident ? if the rolled-back version also crashes, the problem isn't version-specific.
- **Auto-remediation opt-in**: utoRemediation: true must be explicitly enabled; default is monitor-only.

### Per-release fine-grained control
- **Event filter**: onUnhealthyOnly, minRestartCount, ignoreEventTypes per release.
- **Independent webhook**: Each HelmRelease pushes to its own endpoint with custom headers.
- **Independent policies**: Each release has its own auto-remediation rules.

---

## Architecture

`
                         kube-apiserver
                              |
               +--------------+--------------+
               |                             |
     HelmRelease Controller          SharedInformer
     (Reconcile: install/            (watch all Helm Pods)
      upgrade/rollback/                    |
      uninstall)                    PodEventQueue
               |                   (UID dedup)
               |                          |
               |                     WorkerPool
               |                   (serial, lock-free)
               |                          |
               +-----------+--------------+
                           |
              +------------+-----------+
              |            |           |
         HTTP Push   CR Status     Prometheus
         (Webhook)   (write-back)  Collector
                                   |
                    +--------------+--------------+
                    |                             |
              Pod metrics                AI metrics
              (Informer)          (Active scrape vLLM)
              phase/ready/         latency/TTFT/tokens/
              restart              GPU-cache/requests
`

---

## Design philosophy

InferGuard does not replace your inference engine. It adds the operational layer:

`
vLLM/TGI:    how to run inference fast
InferGuard:  how to keep inference running

Helm's --wait --atomic:   deploy-time (install ? deployed, then exits)
InferGuard:              runtime (deployed ? monitor ? unhealthy ? auto-rollback)
`

The Prometheus Collector uses **active scrape**, not Informer cache, for AI metrics. When Prometheus hits /metrics, InferGuard:

1. Iterates Informer store for pod-level state (Phase/Ready/Restart)
2. Identifies registered inference pods (via ReleaseRegistry)
3. HTTP GETs http://<podIP>:8000/metrics from each inference pod
4. Parses vLLM/TGI-native Prometheus metrics
5. Re-exposes them under the inferguard_ prefix

This means **zero additional exporters** ? no 
vidia-dcgm-exporter, no ServiceMonitor per model. InferGuard IS the exporter.

---

## Comparison

| | InferGuard | Flux CD | Argo CD | KServe | Raw Helm |
|---|:---:|:---:|:---:|:---:|:---:|
| Model deploy via CRD | ? | ? | ? | ? | ? |
| Runtime Pod monitoring | ? | ? | ? | ? | --wait |
| Active scrape AI metrics | ? | ? | ? | ? | ? |
| Auto-rollback on latency | ? | ? | ? | ? | --atomic |
| Per-release event filter | ? | ? | ? | ? | ? |
| CR Status writes Pod state | ? | ? | ? | ? | ? |
| Deployment complexity | Single binary | 4+ components | 3+ components | 3+ components | CLI only |
| Learning curve | Low | High | High | Medium | Low |

**Positioning**: InferGuard is not a Flux/ArgoCD replacement ? it doesn't do GitOps. It fills the gap between Helm CLI and GitOps tools: **CR as the control plane, Pod runtime state as the judgment, auto-rollback as the safety net.**

---

## HelmRelease (ModelRelease) CRD

### Full example

`yaml
apiVersion: inferguard.io/v1alpha1
kind: HelmRelease
metadata:
  name: llama-3-8b
spec:
  releaseName: llama-3-8b
  chart:
    repository: https://charts.mycompany.com
    name: vllm-server
    version: "1.2.0"
  values:
    modelName: "meta-llama/Meta-Llama-3-8B-Instruct"
    replicas: 2
    resources:
      limits:
        nvidia.com/gpu: 1
  atomic: true
  waitTimeout: 600
  podMonitor:
    enabled: true
    endpoint: "https://alerts.ai-platform.com/webhook"
    autoRemediation: true       # enable policy-driven auto-rollback
    filter:
      onUnhealthyOnly: true
      minRestartCount: 3
  policies:
    - name: auto-rollback-on-latency
      condition:
        type: PodRestart
        threshold: 5
      action:
        type: Rollback
        notify: true
`

### Key fields

| Category | Field | Description |
|----------|-------|-------------|
| Chart | chart.repository / chart.name / chart.version | Remote Helm repo |
| Chart | chart.localPath | Local .tgz or directory |
| Helm | alues | Passed to Helm .Values |
| Helm | tomic | Auto-rollback on upgrade failure |
| Helm | orceUpgrade | Force update immutable resources |
| Helm | 	argetRevision | Trigger rollback (auto-cleared on success) |
| Helm | waitTimeout | Timeout in seconds (default 300) |
| Monitor | podMonitor.enabled | Enable pod monitoring |
| Monitor | podMonitor.endpoint | Webhook push URL |
| Monitor | podMonitor.autoRemediation | Enable auto-rollback policies (default false) |
| Filter | podMonitor.filter.onUnhealthyOnly | Only push unhealthy events |
| Filter | podMonitor.filter.minRestartCount | Restart threshold |
| Filter | podMonitor.filter.ignoreEventTypes | Event types to skip |
| Policy | policies[].condition.type | PodRestart / PodNotReady / PodCrash |
| Policy | policies[].condition.threshold | Trigger threshold |
| Policy | policies[].action.type | Rollback / Notify |

---

## Prometheus metrics

### Pod metrics (source: Informer)

| Metric | Labels | Description |
|--------|--------|-------------|
| inferguard_pod_info | namespace, name, release, phase, node, pod_ip | Pod metadata |
| inferguard_pod_ready | namespace, name, release | 1=Ready, 0=Not |
| inferguard_pod_restart_total | namespace, name, release | Cumulative restarts |
| inferguard_pod_events_total | event_type | Events processed by type |

### AI inference metrics (source: active scrape of vLLM/TGI /metrics)

| Metric | Labels | Description |
|--------|--------|-------------|
| inferguard_inference_latency_seconds | namespace, pod, release, model | Per-output-token latency (sum) |
| inferguard_inference_time_to_first_token_seconds | namespace, pod, release, model | TTFT latency (sum) |
| inferguard_inference_requests_total | namespace, pod, release, model | Successful requests |
| inferguard_inference_requests_running | namespace, pod, release | Currently running requests |
| inferguard_inference_requests_waiting | namespace, pod, release | Queued requests |
| inferguard_inference_tokens_total | namespace, pod, release, kind | Tokens (kind=prompt\|generation) |
| inferguard_inference_gpu_cache_usage_percent | namespace, pod, release | GPU KV cache usage |

**How it works**: Prometheus scrapes /metrics ? InferGuard Collector actively HTTP GETs each inference pod's /metrics ? parses vLLM metrics ? re-exposes them. No Informer cache, no extra exporters.

---

## Push event format

`json
{
  "type": "MODIFIED",
  "pod": {
    "namespace": "production", "name": "llama-3-8b-abc123",
    "phase": "Running", "ready": true, "restart": 0
  },
  "releaseName": "llama-3-8b",
  "timestamp": 1720456789
}
`

| Field | Description |
|-------|-------------|
| 	ype | ADDED / MODIFIED / DELETED |
| pod | API server live state (empty for DELETED) |
| oldPod | Only present in DELETED ? last known state |
| 
eleaseName | Owning HelmRelease name |

---

## Usage

`ash
# Deploy the operator
kubectl apply -f charts/inferguard/templates/crd.yaml
kubectl apply -k config/default

# Create a model release
kubectl apply -f model-release.yaml

# Check status
kubectl get helmrelease llama-3-8b -o yaml

# Scrape metrics
curl http://inferguard-metrics:9090/metrics
`

---

## Project structure

`
api/v1alpha1/              CRD types: HelmRelease + Policy/Filter specs
cmd/manager/main.go        Entry point, wires all components
internal/
  controller/              Reconcile loop + Helm operations + CR status
  helm/                    Helm v3 SDK (install/upgrade/rollback/uninstall + chart download/local)
pkg/
  podwatch/                Pod monitoring: Watcher, Queue, Worker, Sender, Collector, Filter
  policy/                  Policy engine: condition matching + action execution
  log/                     zap logger
charts/inferguard/         Helm Chart for operator deployment
`

## Tech stack

Go 1.21+ ? controller-runtime v0.17 ? client-go v0.29 ? Helm SDK v3.14 ? Prometheus client_golang v1.18

---

> **InferGuard ? Deploy, monitor, and auto-heal AI inference on Kubernetes.**
