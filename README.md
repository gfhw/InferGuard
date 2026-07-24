# InferGuard

AI 推理 Pod 全生命周期管理 Operator —— 声明式部署、实时监控、Prometheus 指标出口、自动回滚自愈。一个 CRD 管到底。

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/dl/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28+-326CE5?style=flat&logo=kubernetes)](https://kubernetes.io/)
[![Helm](https://img.shields.io/badge/Helm-3.14+-0F1689?style=flat&logo=helm)](https://helm.sh/)

---

## 解决了什么问题

在 Kubernetes 上部署 AI 模型远不止 `helm install` 这么简单。一个模型 Pod 要经历 GPU 调度、权重下载（5GB+）、显存分配、预热推理——然后 7x24 小时对外服务。任何时刻都可能 GPU OOM、推理延迟飙升、或者静默退化。

**InferGuard 管理 AI 推理 Pod 的完整生命周期。** 从 Helm 部署到运行时监控再到自动回滚，一个 CRD 闭环。

Helm 的 `--wait --atomic` 只管到「部署成功那一刻」，之后 Pod 崩了 Helm 不管。InferGuard 把 Helm 拉进了 Kubernetes 的 Reconcile 循环——让 Helm Release 像 Deployment 一样具备自愈能力。

---

## 核心能力

### Helm 生命周期声明式管理

| 操作 | 方式 |
|------|------|
| 部署模型 | 创建 CR → Operator 执行 `helm install` |
| 升级 | 修改 `values` 或 `chart.version` → 自动 `helm upgrade` |
| 回滚 | 设置 `targetRevision` → `helm rollback`，成功后自动对齐 spec |
| 卸载 | 删除 CR → Finalizer 触发 `helm uninstall`，释放 GPU 资源 |
| 本地 Chart | `chart.localPath` 直接加载 `.tgz` 或目录 |

### AI 推理实时监控

- **Prometheus 专做 AI 指标**（推理延迟、TTFT、Token 吞吐、GPU KV-Cache）。Pod 状态（Phase / Ready / Restart）交给 Webhook Push 和 CR Status。
- **主动抓取**：Prometheus 每次 scrape 时，Collector 实时 HTTP GET 每个推理 Pod 的 `/metrics`（vLLM / TGI / SGLang），零 Informer 缓存依赖。
- **三路输出**：HTTP Webhook Push（告警）+ Prometheus Pull（Grafana）+ CR Status 回写（kubectl 直接看）。

### 智能自愈

- **策略引擎**：声明式条件（PodRestart > 5 → Rollback），在 Pod 事件路径内嵌评估。
- **错误分类**：区分 chart 问题（不重试）和网络抖动（指数退避，最多 10 次）。
- **一命护盾**：策略自动回滚只触发一次——回滚后还崩说明不是版本问题，保持失败态等人介入。
- **显式开关**：`autoRemediation: true` 必须显式开启，默认只监控不动手。

### Per-release 精细化控制

- **事件过滤器**：`onUnhealthyOnly` / `minRestartCount` / `ignoreEventTypes` 按 release 独立配置。
- **独立 Webhook**：每个 HelmRelease 可推送到不同的 URL，带自定义 headers。
- **独立策略**：每个 release 可配不同的自愈规则。

---

## 架构

```
                         kube-apiserver
                              |
               +--------------+--------------+
               |                             |
     HelmRelease Controller          SharedInformer
     (Reconcile: install/            (watch Helm Pods)
      upgrade/rollback/                    |
      uninstall)                    PodEventQueue
               |                   (UID 去重)
               |                          |
               |                     WorkerPool
               |                   (串行, 无锁)
               |                          |
               +-----------+--------------+
                           |
              +------------+-----------+
              |            |           |
         HTTP Push   CR Status     Prometheus
         (Webhook)   (回写CR)      Collector
                                   |
                          HTTP GET podIP:8000/metrics
                          (vLLM / TGI / SGLang 实时抓取)
```

---

## 设计哲学

```
vLLM / TGI:   怎么推理得快
InferGuard:   怎么运维得稳

Helm --wait --atomic:   部署时（install → deployed，结束）
InferGuard:             运行时（deployed → monitor → 不健康 → 自动 rollback）
```

**三路输出各司其职：**

| 通道 | 数据 | 用途 |
|------|------|------|
| Webhook Push | Pod 状态变更事件 | 对接告警系统（飞书、钉钉、PagerDuty） |
| Prometheus Pull | AI 推理指标（延迟/吞吐/GPU） | Grafana 大盘、告警规则 |
| CR Status | Pod 运行时状态 + 推理指标概要 | kubectl 直接查看，不依赖外部系统 |

---

## 与业界工具对比

| | InferGuard | Flux CD | Argo CD | KServe | 原生 Helm |
|---|:---:|:---:|:---:|:---:|:---:|
| CRD 部署模型 | ✅ | ✅ | ✅ | ✅ | ❌ |
| 运行时 Pod 监控 | ✅ | ❌ | ❌ | ✅ | `--wait` |
| 主动抓取 AI 推理指标 | ✅ | ❌ | ❌ | ❌ | ❌ |
| 推理延迟超标自动回滚 | ✅ | ✅ | ✅ | ❌ | `--atomic` |
| Per-release 事件过滤 | ✅ | ❌ | ❌ | ❌ | ❌ |
| CR Status 回写 Pod 状态 | ✅ | ❌ | ❌ | ❌ | ❌ |
| 部署复杂度 | 单二进制 | 4+ 组件 | 3+ 组件 | 3+ 组件 | 纯 CLI |
| 学习成本 | 低 | 高 | 高 | 中 | 低 |

**定位**：InferGuard 不做 GitOps（那是 Flux/ArgoCD 的事），它填补的是 Helm CLI 和 GitOps 工具之间的空白——以 CR 为操作界面、以 Pod 运行时状态为判断依据、以自动回滚为兜底。

---

## HelmRelease CRD 示例

```yaml
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
    # 本地 chart: localPath: /charts/vllm-1.2.0.tgz
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
    autoRemediation: true       # 开启策略自愈（默认 false，仅监控）
    filter:
      onUnhealthyOnly: true
      minRestartCount: 3
  policies:
    - name: auto-rollback-on-crash
      condition:
        type: PodRestart
        threshold: 5
      action:
        type: Rollback
        notify: true
```

### 关键字段速查

| 分类 | 字段 | 说明 |
|------|------|------|
| Chart | `chart.repository` / `chart.name` / `chart.version` | 远程 Helm 仓库 |
| Chart | `chart.localPath` | 本地 `.tgz` 或目录 |
| Helm | `values` | 传给 Helm `.Values` |
| Helm | `atomic` | 升级失败自动回滚 |
| Helm | `forceUpgrade` | 强制更新不可变资源 |
| Helm | `targetRevision` | 触发回滚（成功后自动清空） |
| Helm | `waitTimeout` | 超时秒数（默认 300） |
| 监控 | `podMonitor.enabled` | 开启 Pod 监控 |
| 监控 | `podMonitor.endpoint` | Webhook 推送地址 |
| 监控 | `podMonitor.autoRemediation` | 开启策略自愈（默认 false） |
| 过滤 | `podMonitor.filter.onUnhealthyOnly` | 只推送不健康事件 |
| 过滤 | `podMonitor.filter.minRestartCount` | restart 阈值 |
| 过滤 | `podMonitor.filter.ignoreEventTypes` | 跳过的事件类型 |
| 策略 | `policies[].condition.type` | PodRestart / PodNotReady / PodCrash |
| 策略 | `policies[].condition.threshold` | 触发阈值 |
| 策略 | `policies[].action.type` | Rollback / Notify |

---

## Prometheus 指标

Prometheus 只暴露 AI 推理指标，Pod 状态指标请走 Webhook Push 或 CR Status 通道。

### AI 推理指标（来源：Prometheus scrape 时主动 HTTP GET vLLM/TGI /metrics）

| 指标 | Labels | 说明 |
|------|--------|------|
| `inferguard_inference_latency_seconds` | namespace, pod, release, model | 逐 token 生成延迟（sum） |
| `inferguard_inference_latency_seconds_count` | namespace, pod, release, model | 逐 token 生成延迟（count） |
| `inferguard_inference_time_to_first_token_seconds` | namespace, pod, release, model | TTFT 首 token 延迟（sum） |
| `inferguard_inference_time_to_first_token_seconds_count` | namespace, pod, release, model | TTFT 首 token 延迟（count） |
| `inferguard_inference_requests_total` | namespace, pod, release, model | 成功请求总数 |
| `inferguard_inference_requests_running` | namespace, pod, release | 当前正在处理的请求数 |
| `inferguard_inference_requests_waiting` | namespace, pod, release | 排队等待的请求数 |
| `inferguard_inference_tokens_total` | namespace, pod, release, kind | Token 总数（kind=prompt\|generation） |
| `inferguard_inference_gpu_cache_usage_percent` | namespace, pod, release | GPU KV-Cache 使用率 |

**工作原理**：Prometheus scrape `/metrics` → InferGuard Collector 对每个已注册的推理 Pod 执行 `HTTP GET podIP:8000/metrics` → 解析 vLLM 原生指标 → 以 `inferguard_` 前缀重新暴露。**零额外 exporter，零 Informer 缓存依赖。**

---

## 推送事件格式

```json
{
  "type": "MODIFIED",
  "pod": {
    "namespace": "production", "name": "llama-3-8b-abc123",
    "phase": "Running", "ready": true, "restart": 0
  },
  "releaseName": "llama-3-8b",
  "timestamp": 1720456789
}
```

| 字段 | 说明 |
|------|------|
| `type` | ADDED / MODIFIED / DELETED |
| `pod` | API Server 实时状态（DELETED 时为空） |
| `oldPod` | 仅在 DELETED 时出现——Pod 删除前最后已知状态 |
| `releaseName` | 所属 HelmRelease 名称 |

---

## 使用方法

```bash
# 部署 Operator
kubectl apply -f charts/inferguard/templates/crd.yaml
kubectl apply -k config/default

# 创建模型部署
kubectl apply -f model-release.yaml

# 查看状态
kubectl get helmrelease llama-3-8b -o yaml

# 抓取 Prometheus 指标
curl http://inferguard-metrics:9090/metrics
```

---

## 项目结构

```
api/v1alpha1/              CRD 类型：HelmRelease + 策略/过滤定义
cmd/manager/main.go        入口，组装全链路
internal/
  controller/              Reconcile 循环 + Helm 操作 + CR Status
  helm/                    Helm v3 SDK (install/upgrade/rollback/uninstall + chart 下载/本地)
pkg/
  podwatch/                Pod 监控：Watcher、去重队列、Worker、推送、Prometheus Collector、过滤
  policy/                  策略引擎：条件匹配 + 动作执行
  log/                     zap 日志
charts/inferguard/         Operator 部署 Helm Chart
```

## 技术栈

`Go 1.21+` · `controller-runtime v0.17` · `client-go v0.29` · `Helm SDK v3.14` · `Prometheus client_golang v1.18`

---

> **InferGuard — AI 推理 Pod 部署、监控、自愈，一个 CRD 管到底。**
