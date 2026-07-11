# Watchpod

基于 Kubernetes Operator 模式实现的 Helm Release 生命周期管理 + Pod 实时监控系统。一个 CRD，一个 Informer，三路输出。

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/dl/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28+-326CE5?style=flat&logo=kubernetes)](https://kubernetes.io/)
[![Helm](https://img.shields.io/badge/Helm-3.14+-0F1689?style=flat&logo=helm)](https://helm.sh/)

---

## 项目定位

Helm 的问题是：`helm install` 返回 `deployed` 不代表 Pod 真的跑起来了。Pod 可能 CrashLoopBackOff、可能被驱逐、可能 restart 了几十次——Helm 不知道，也不关心。

Watchpod 填补"chart 已部署"到"Pod 确实健康运行"之间的监控空白。通过一个自定义 `HelmRelease` CRD，你声明一个 Helm chart 的部署意图，Operator 负责执行部署，并用 SharedInformer 持续盯住所有 Pod，状态变更实时推送到三个方向。

---

## 核心能力

| 能力 | 说明 |
|------|------|
| **Helm 生命周期管理** | 创建 CR → `helm install`，改 values/version → `helm upgrade`，设 targetRevision → `helm rollback`，删除 CR → `helm uninstall` |
| **Pod 实时监控** | 单个 SharedInformer 监听所有 `app.kubernetes.io/managed-by=Helm` 的 Pod，不轮询、不遗漏 |
| **事件削峰去重** | 同一 Pod 短时间内多次事件在队列中合并为一条，防止 API 重复查询和下游重复推送 |
| **两阶段验证** | Informer 负责"唤醒"，API Server 负责"验真"——推送的是实时查到的状态，不是可能过期的缓存 |
| **三路输出** | HTTP Webhook Push（JSON 事件）+ Prometheus Pull（`/metrics`）+ CR Status 回写（`.status.podStatuses`） |
| **指数退避重试** | 部署失败自动重试（1s → 2s → 4s → ... → 10min 上限），最多 5 次后永久停止 |
| **Per-release 配置** | 每个 HelmRelease 可独立配置 webhook 地址、HTTP 方法、自定义 headers |
| **本地 chart 支持** | 支持远程 Helm 仓库和本地 chart 路径（`.tgz` 或目录）两种部署方式 |

---

## 架构

```
                        kube-apiserver
                             |
                  ONE SharedInformer
              (label: app.kubernetes.io/managed-by=Helm)
                             |
                       PodEventQueue
                   (key = pod.UID, 削峰去重)
                             | notify（非阻塞）
                       WorkerPool
                  (单 goroutine 串行消费)
                             |
                  ReleaseRegistry 查表路由
              (per-release EventSender + StatusUpdater)
                             |
                  +----------+----------+
                  |          |          |
              HTTP Push  Prometheus  CR Status
```

**无轮询、无 goroutine 爆炸、事件风暴不扩散。**

---

## 关键设计决策

### 1. Informer 回调与 Worker 分离

Informer 的事件回调（AddFunc / UpdateFunc / DeleteFunc）跑在 SharedInformer 内部的同一个 goroutine 里。如果在回调中直接做 I/O（API Get + HTTP Push + CR Update），单个 Pod 的处理延迟会阻塞所有其他 Pod 的事件。

解决方案：回调只做入队（微秒级），独立 Worker goroutine 串行消费（允许 I/O 慢）。两条时间线彻底解耦。

### 2. UID 去重削峰

Pod 反复 CrashLoopBackOff 时，Informer 会在短时间内产生大量事件。PodEventQueue 以 Pod UID 为 key——同一 UID 在队列中最多存在一条，后续事件直接丢弃。Worker 消费时从 API Server 获取实时状态，保证推送的是最新数据。

`DrainAll()` 消费完成后 index 清空，该 UID 可以再次入队。去重是"削峰"而非"终身一次"。

### 3. 两阶段事件验证

Informer 告知"有变化发生了"，API Server 告知"当前真实状态是什么"：

| 事件来源 | 处理方式 |
|---------|---------|
| ADDED / MODIFIED | 实时 Get Pod -> 推送 API 返回的当前状态 |
| API 返回 NotFound | 用入队快照合成 DELETED 事件 |
| DELETED（Informer 通知） | 直接信任 Informer，立即推送 |

这解决了 Informer 缓存延迟导致的经典问题——例如 Pod 已被删除但缓存里还是 Running 状态。

### 4. 全局单 Informer

所有 HelmRelease 共享一个 `SharedInformerFactory`，按 `app.kubernetes.io/managed-by=Helm` 标签过滤。不是每个 CR 一个 Informer。release 级别的路由（推哪个 webhook、更新哪个 CR）由 ReleaseRegistry 在处理时动态查表。

### 5. 串行无锁处理

单 goroutine 逐条消费队列。无锁、无 WaitGroup、无竞态。

### 6. 初始列表静默丢弃

启动时 Informer 列出集群中所有符合条件的 Pod 并逐条触发 `AddFunc`。这些全部通过 `HasSynced()` 检查丢弃。WorkerPool 在 `WaitForCacheSync` 完成后才启动，不会消费任何初始事件。

### 7. 指数退避重试

Reconcile 失败时自动重试，间隔按指数增长：1s -> 2s -> 4s -> 8s -> ...，上限 10 分钟。重试次数写入 CR 的 `.status.retryCount`，达到 5 次后标记永久失败，不再重试。当 CR spec 的 Generation 变化时（用户修改了配置），重试计数重置为 0。

### 8. 成功不轮询

CR 处于 Running 且 spec 未变更时（`ObservedGeneration == Generation`），不再 requeue。只有用户修改 spec 触发新 Generation 时才重新 Reconcile。避免成功部署后无意义的周期检查，也不会给 Helm revision 制造垃圾。

---

## 业界对比

| 工具 | Helm 生命周期 | Push 事件 | 实时 Pod 监控 | 去重 | API 验证 |
|------|:---:|:---:|:---:|:---:|:---:|
| kube-state-metrics | - | - | - | - | - |
| event-exporter | - | Yes | - | - | - |
| Prometheus + Alertmanager | - | - | Yes | - | - |
| Flux / ArgoCD | Yes | - | - | - | - |
| **Watchpod** | Yes | Yes | Yes | Yes | Yes |

---

## HelmRelease CRD

### 完整示例：远程仓库

```yaml
apiVersion: helm.watchpod.io/v1alpha1
kind: HelmRelease
metadata:
  name: my-nginx
  namespace: production
spec:
  releaseName: my-nginx
  namespace: production
  chart:
    repository: https://charts.bitnami.com/bitnami
    name: nginx
    version: "15.10.3"
  values:
    replicaCount: 3
    service:
      type: ClusterIP
  atomic: true
  forceUpgrade: false
  waitTimeout: 300
  podMonitor:
    enabled: true
    endpoint: "https://alerts.mycompany.com/webhook/pod-events"
    method: "POST"
    headers:
      Authorization: "Bearer eyJhbGciOi..."
      X-Environment: "production"
```

### 完整示例：本地 chart

```yaml
spec:
  chart:
    localPath: /charts/my-nginx-15.10.3.tgz   # 与 repository/name/version 互斥
  values:
    replicaCount: 3
  podMonitor:
    enabled: true
    endpoint: "https://alerts.mycompany.com/webhook"
```

### Spec 字段

| 字段 | 必填 | 说明 |
|-------|:---:|------|
| `releaseName` | 否 | Helm release 名称（默认取 `metadata.name`） |
| `namespace` | 否 | 部署目标 namespace（默认取 CR 所在 namespace） |
| `chart.repository` | 条件 | Helm 仓库 URL（远程模式必填） |
| `chart.name` | 条件 | Chart 名称（远程模式必填） |
| `chart.version` | 否 | Chart 版本，修改会触发 upgrade |
| `chart.localPath` | 条件 | 本地 chart 路径（与 repository 互斥） |
| `values` | 否 | 透传给 Helm `.Values` 的任意 YAML/JSON |
| `atomic` | 否 | upgrade 失败自动 rollback（默认 false） |
| `forceUpgrade` | 否 | 强制更新不可变资源（默认 false） |
| `targetRevision` | 否 | 设为具体版本号触发 rollback（成功后自动清空） |
| `waitTimeout` | 否 | Helm 操作超时秒数（默认 300，即 5 分钟） |
| `podMonitor.enabled` | 否 | 是否开启 Pod 监控 |
| `podMonitor.endpoint` | 否 | Webhook 推送地址 |
| `podMonitor.method` | 否 | HTTP 方法（默认 POST） |
| `podMonitor.headers` | 否 | 自定义 HTTP headers |

### Status 字段

| 字段 | 说明 |
|------|------|
| `phase` | 当前阶段：Pending / Installing / Upgrading / Running / Failed |
| `releaseStatus` | Helm release 状态 |
| `revision` | 当前 Helm revision 号 |
| `observedGeneration` | 最后一次成功 Reconcile 时对应的 Generation |
| `lastAttemptedGeneration` | 最后一次尝试 Reconcile 时对应的 Generation |
| `retryCount` | 连续失败次数（成功时重置为 0，达到 5 次停止重试） |
| `lastFailureMessage` | 最近一次失败的详细错误信息 |
| `podStatuses` | 实时 Pod 状态列表，每个 Pod 包含 namespace/name/uid/phase/nodeName/podIP/ready/restart |

---

## 使用方法

### 1. 部署 Operator

```bash
kubectl apply -f charts/watchpod-operator/templates/crd.yaml
kubectl apply -k config/default
```

### 2. 创建 HelmRelease

```bash
kubectl apply -f - <<EOF
apiVersion: helm.watchpod.io/v1alpha1
kind: HelmRelease
metadata:
  name: my-app
spec:
  chart:
    repository: https://charts.bitnami.com/bitnami
    name: nginx
    version: "15.10.3"
  values:
    replicaCount: 2
  podMonitor:
    enabled: true
    endpoint: "http://my-monitoring:8080/webhook"
EOF
```

### 3. 查看状态

```bash
# CR 整体状态
kubectl get helmrelease my-app -o yaml

# Pod 实时状态
kubectl get helmrelease my-app -o jsonpath='{.status.podStatuses}' | jq
```

### 4. 升级

修改 `values` 或 `chart.version` 后 `kubectl apply`，Operator 自动执行 `helm upgrade`。

### 5. 回滚

```bash
kubectl patch helmrelease my-app --type=merge -p '{"spec":{"targetRevision":"2"}}'
```

回滚成功后 `targetRevision` 自动清空，`chart.version` 自动更新为实际回滚到的版本。

### 6. 卸载

```bash
kubectl delete helmrelease my-app
```

Operator 的 finalizer 会自动执行 `helm uninstall` 清理所有资源。

---

## Push 事件格式

```json
{
  "type": "MODIFIED",
  "pod": {
    "namespace": "production",
    "name": "my-nginx-7d4f8b9c-xk2m9",
    "uid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
    "phase": "Running",
    "nodeName": "worker-3",
    "podIP": "10.244.3.15",
    "ready": true,
    "restart": 0
  },
  "oldPod": {
    "phase": "Pending",
    "ready": false,
    "restart": 0
  },
  "namespace": "production",
  "releaseName": "my-nginx",
  "timestamp": 1720456789
}
```

| 字段 | 来源 | 说明 |
|------|------|------|
| `type` | Informer | ADDED / MODIFIED / DELETED |
| `pod` | API Get | 实时状态（DELETED 事件时为空） |
| `oldPod` | 入队快照 | 事件首次进入队列时的状态 |
| `releaseName` | Pod label | 从 `app.kubernetes.io/instance` 提取 |

---

## Prometheus 指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `watchpod_pod_info` | Gauge | namespace, name, release, phase, node, pod_ip, event_type | Pod 元数据和当前 Phase |
| `watchpod_pod_ready` | Gauge | namespace, name, release | 1 = Ready，0 = Not Ready |
| `watchpod_pod_restart_total` | Gauge | namespace, name, release | Pod 累计 restart 次数 |
| `watchpod_pod_events_total` | Counter | event_type | 按类型统计已处理的事件数 |

---

## 项目结构

```
api/v1alpha1/                  CRD 类型定义 + DeepCopy + 辅助方法
cmd/manager/main.go            入口，组装 Manager + GlobalPodWatcher + Reconciler
internal/
  controller/                  Reconcile 循环 + PodMonitor 管理 + CR Status 更新
  helm/                        Helm v3 SDK 封装（install / upgrade / rollback / uninstall / chart 下载 / 本地加载）
pkg/
  podwatch/
    watcher.go                 GlobalPodWatcher：共享 Informer + Prometheus HTTP server
    queue.go                   PodEventQueue：UID 去重 + 非阻塞通知 + batch DrainAll
    worker.go                  WorkerPool：串行消费 + 两阶段 API 验证 + push + CR 更新
    sender.go                  EventSender：HTTP POST + 自定义 method/headers
    collector.go               PodCollector：实现 prometheus.Collector 接口
    filter.go                  Filter：标签过滤 + release 名提取
    types.go                   共享类型：PodInfo / PodEvent / ReleaseRegistry / StatusUpdater
    config.go                  配置结构体
  log/                         zap 日志封装
charts/watchpod-operator/      Helm Chart：CRD + RBAC + Deployment + ServiceAccount
config/                        Kustomize 配置
build/Dockerfile               Docker 多阶段构建
```

---

## 技术栈

`Go 1.21+` · `controller-runtime v0.17` · `client-go v0.29` · `Helm SDK v3.14` · `Prometheus client_golang v1.18` · `zap`

---

## 构建与部署

```bash
# 构建 Docker 镜像
make build

# 推送到镜像仓库
export REGISTRY=docker.io/myuser
make push

# 部署到集群
kubectl apply -f charts/watchpod-operator/templates/crd.yaml
kubectl apply -k config/default
```

需要 Go 1.21+、Kubernetes 1.28+、Helm 3.14+、Docker。

---

## 设计理念

- **Informer 当闹钟，API 当真相源**——缓存不可信，推送前必须实时验证
- **UID 去重削峰**——防止事件风暴堆积，每次处理都带实时 API 状态
- **一个 Informer 盯所有 Pod**——不做 per-release Informer，路由在消费端完成
- **成功不轮询，失败退避停**——不浪费集群资源，不给 Helm revision 制造垃圾
- **单 CRD 管两件事**——Helm 生命周期 + Pod 监控，一个 `kubectl apply` 全搞定