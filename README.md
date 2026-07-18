# Watchpod

基于 Kubernetes Operator 模式实现的 Helm Release 声明式管理 + Pod 实时监控 + 策略自愈平台。一个 CRD，全闭环。

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/dl/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28+-326CE5?style=flat&logo=kubernetes)](https://kubernetes.io/)
[![Helm](https://img.shields.io/badge/Helm-3.14+-0F1689?style=flat&logo=helm)](https://helm.sh/)

---

## 项目定位

Helm 的问题是：`helm install` 返回 `deployed` 不代表 Pod 真的跑起来了。Pod 可能 CrashLoopBackOff、可能被驱逐、可能 restart 了几十次——Helm 不知道，也不关心。

Watchpod 不仅填补"chart 已部署"到"Pod 确实健康运行"之间的监控空白，更进一步——**当 Pod 异常时自动执行回滚、降级等自愈动作**。通过一个 `HelmRelease` CRD，实现从部署、监控到自愈的完整闭环。

---

## 核心能力

| 能力 | 说明 |
|------|------|
| **Helm 生命周期管理** | 创建 CR → `helm install`，改 values/version → `helm upgrade`，设 targetRevision → `helm rollback`，删除 CR → `helm uninstall` |
| **Pod 实时监控** | 单个 SharedInformer 监听所有 `app.kubernetes.io/managed-by=Helm` 的 Pod，不轮询、不遗漏 |
| **事件削峰去重** | 同一 Pod 短时间内多次事件在队列中合并为一条，防止 API 重复查询和下游重复推送 |
| **两阶段验证** | Informer 负责"唤醒"，API Server 负责"验真"——推送的是实时查到的状态，不是可能过期的缓存 |
| **三路输出** | HTTP Webhook Push（JSON 事件，支持 per-release 过滤）+ Prometheus Pull（`/metrics`）+ CR Status 回写（`.status.podStatuses`） |
| **智能重试** | 区分永久失败（chart 有问题，不重试）和瞬时故障（网络超时，退避重试），两套独立重试上限 |
| **事件过滤策略** | Per-release 可配置过滤规则：只推不健康事件、按 restart 阈值过滤、忽略指定 event type |
| **策略自愈引擎** | 声明式策略：Pod restart > 5 → 自动 rollback，Pod Crash → 紧急通知，从"告警"升级为"自愈" |
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
          +------------------+------------------+
          |                  |                  |
    ReleaseRegistry     EventFilter       PolicyEngine
    (per-release路由)   (CR可配过滤)      (条件→动作)
          |                                     |
   HTTP Push + Prometheus + CR Status       Auto-Rollback
                                                    |
                                              EventSender
```

**从部署到监控到自愈，全链路闭环。**

---

## 关键设计决策

### 1. Informer 回调与 Worker 分离

Informer 的事件回调跑在 SharedInformer 内部的同一个 goroutine 里。如果在回调中直接做 I/O，单个 Pod 的处理延迟会阻塞所有其他 Pod 的事件。回调只做入队（微秒级），独立 Worker goroutine 串行消费（允许 I/O 慢）。

### 2. UID 去重削峰

Pod 反复 CrashLoopBackOff 时，PodEventQueue 以 Pod UID 为 key——同一 UID 在队列中最多存在一条，后续事件直接丢弃。Worker 消费时从 API Server 获取实时状态，`DrainAll()` 后 index 清空可再次入队。是"削峰"而非"终身一次"。

### 3. 两阶段事件验证

Informer 告知"有变化发生了"，API Server 告知"当前真实状态是什么"——推送的是实时查到的状态，不是可能过期的缓存。

### 4. 智能错误分类重试

区分永久失败和瞬时故障：

| 错误类型 | 示例 | 重试策略 |
|---------|------|---------|
| 永久失败 | chart 不存在、values 语法错误、模板渲染错误 | 不重试，直接标记失败 |
| 瞬时故障 | kube-apiserver 超时、网络闪断 | 指数退避：1s→2s→4s→...→10min，最多 10 次 |

### 5. 策略自愈引擎

在上报链路中插入策略评估点。用户声明条件+动作规则，Worker 消费每条 Pod 事件时自动评估。触发后执行 Helm rollback 或 webhook 通知，执行结果通过现有 EventSender 推送。

```
Pod 状态变更 → Worker 处理 → 过滤 → 推送
                                   └→ PolicyEngine.Evaluate
                                       └→ 匹配条件? → Rollback/Notify
```

### 6. Per-release 事件过滤

在 CR 中配置过滤规则，从源头减少告警噪音。支持按健康状态、restart 阈值、event type、Pod phase 四个维度过滤。规则存储在 ReleaseRegistry 中，Worker 推送前判断。

### 7. 全局单 Informer + 成功不轮询

所有 HelmRelease 共享一个 Informer，release 路由在处理时动态查表。CR 处于 Running 且 spec 未变更时不再 requeue，避免无意义的 Helm upgrade。

---

## HelmRelease CRD

### 完整示例：策略自愈 + 事件过滤

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
  atomic: true
  waitTimeout: 300
  podMonitor:
    enabled: true
    endpoint: "https://alerts.mycompany.com/webhook"
    method: "POST"
    headers:
      Authorization: "Bearer xxx"
    filter:
      onUnhealthyOnly: true       # 只在 Pod 不健康时推送
      minRestartCount: 3          # restart >= 3 才推送
      ignoreEventTypes:
        - "ADDED"                 # 跳过新 Pod 创建事件
  policies:
    - name: auto-rollback-on-crash
      condition:
        type: PodRestart          # 条件：Pod 重启次数
        threshold: 5              # 超过 5 次
        window: 10m               # 在 10 分钟内
      action:
        type: Rollback            # 动作：自动回滚
        notify: true              # 执行后发通知
    - name: emergency-escalate
      condition:
        type: PodCrash            # 条件：Pod Crash
        threshold: 1
      action:
        type: Notify              # 动作：紧急通知
```

### Spec 字段

| 字段 | 必填 | 说明 |
|-------|:---:|------|
| `releaseName` | 否 | Helm release 名称（默认取 `metadata.name`） |
| `namespace` | 否 | 部署目标 namespace |
| `chart.repository` | 条件 | Helm 仓库 URL（远程模式） |
| `chart.name` | 条件 | Chart 名称 |
| `chart.version` | 否 | Chart 版本，修改触发 upgrade |
| `chart.localPath` | 条件 | 本地 chart 路径（与 repository 互斥） |
| `values` | 否 | 透传给 Helm `.Values` 的 YAML/JSON |
| `atomic` | 否 | upgrade 失败自动 rollback |
| `forceUpgrade` | 否 | 强制更新不可变资源 |
| `targetRevision` | 否 | 触发 rollback（成功后自动清空） |
| `waitTimeout` | 否 | Helm 操作超时秒数（默认 300） |
| `podMonitor.enabled` | 否 | 是否开启 Pod 监控 |
| `podMonitor.endpoint` | 否 | Webhook 推送地址 |
| `podMonitor.method` | 否 | HTTP 方法（默认 POST） |
| `podMonitor.headers` | 否 | 自定义 HTTP headers |
| `podMonitor.filter.onUnhealthyOnly` | 否 | 只推不健康 Pod |
| `podMonitor.filter.minRestartCount` | 否 | restart 低于此值不推 |
| `podMonitor.filter.ignoreEventTypes` | 否 | 跳过的事件类型 |
| `podMonitor.filter.phases` | 否 | 只推指定 phase |
| `policies[].name` | 否 | 策略名称 |
| `policies[].condition.type` | 是 | 条件类型：PodRestart / PodNotReady / PodCrash |
| `policies[].condition.threshold` | 是 | 触发阈值 |
| `policies[].action.type` | 是 | 动作类型：Rollback / Notify |

---



## 使用方法

```bash
# 部署 Operator
kubectl apply -f charts/watchpod-operator/templates/crd.yaml
kubectl apply -k config/default

# 创建带策略的 HelmRelease
kubectl apply -f helmrelease.yaml

# 查看状态
kubectl get helmrelease my-nginx -o yaml
```

## Push 事件格式

```json
{
  "type": "MODIFIED",
  "pod": {
    "namespace": "production", "name": "my-nginx-xxx",
    "phase": "Running", "ready": true, "restart": 0
  },
  "releaseName": "my-nginx",
  "timestamp": 1720456789
}
```

| 字段 | 说明 |
|------|------|
| `type` | ADDED / MODIFIED / DELETED |
| `pod` | 实时状态（DELETED 事件为空） |
| `oldPod` | 仅在 DELETED 事件中出现，Pod 被删除前的最后已知状态 |
| `releaseName` | 所属 HelmRelease |

策略触发后，回滚/通知动作会通过现有 EventSender 执行，后续 Pod 状态变更自然产生对应事件。

---


## 业界对比

| | Watchpod | Flux CD | ArgoCD | Robusta | kube-state-metrics |
|------|:---:|:---:|:---:|:---:|:---:|
| Helm Release CRD | ✅ | ✅ | ✅（Application + Helm） | — | — |
| Pod 实时状态 Push | ✅ | — | — | ✅ | —（Pull only） |
| 策略自愈引擎 | ✅ | ✅（remediation） | ✅（auto-sync） | ✅（playbooks） | — |
| Per-release 过滤 | ✅ | — | — | — | — |
| CR Status 回写 Pod 状态 | ✅ | — | — | — | — |
| 部署复杂度 | 单二进制 | 4+ 组件 | 3+ 组件 | Helm + SaaS | 单 Deployment |
| GitOps | — | ✅ | ✅ | — | — |
| 学习成本 | 低 | 高 | 高 | 中 | 低 |

### 各工具定位

**Flux CD**：功能最全的 CNCF 毕业项目，生产验证充分。但需要装 Source Controller、Kustomize Controller、Helm Controller、Notification Controller 四个组件。Flux 帮你管 Helm 但不盯 Pod 运行时状态——部署完告诉你 "reconciled"，Pod 崩了 Flux 不知道。

**ArgoCD**：UI 漂亮、GitOps 标杆。但它盯的是 Kubernetes 资源的同步状态（Deployment desired == actual），不盯 Pod 的运行时指标（restart 次数、是否 OOMKilled）。Pod 崩了 ArgoCD 看到的 Deployment 仍然是 healthy——spec 没变。

**Robusta**：专做 Pod 监控 + 自动化排障，开箱即用的 playbook 很丰富。但它不管理 Helm 生命周期——你还需要另外的东西部署 chart。

**kube-state-metrics**：Prometheus 生态基础组件，只暴露指标不推送事件。需要自己搭 Prometheus + Alertmanager + Grafana 才能完成 Watchpod 一条 webhook 做到的事。

### Watchpod 的定位

Watchpod 不是 Flux/ArgoCD 的替代品，而是它们的互补品——管完部署后接着盯 Pod、Filter 噪音、自动回滚。核心差异：

- **集成度**：Helm 生命周期 + Pod 监控 + 自愈策略在一个二进制里，声明一个 CR 全自动
- **粒度**：盯的是 Pod 运行时状态（restart、phase、ready），不是 Deployment spec 漂移
- **轻量**：没有 GitOps 包袱，适合不想引入 Flux 复杂度的中小团队
- **策略引擎**：直接在 Pod 事件链路上插入判断点，复用现有 EventSender 和 HelmManager
## 项目结构

```
api/v1alpha1/              CRD 类型 + 策略/过滤类型定义
cmd/manager/main.go        入口，注入 PolicyEngine
internal/
  controller/              Reconcile 循环 + Helm 操作 + CR Status
  helm/                    Helm v3 SDK 封装
pkg/
  podwatch/                Pod 监控核心：watcher、queue、worker、sender、collector、filter、types
  policy/                  策略引擎：条件匹配 + 动作执行
  log/                     zap 日志
charts/watchpod-operator/  Helm Chart 部署资源
config/                    Kustomize 配置
build/Dockerfile           多阶段构建
```

---

## 技术栈

`Go 1.21+` · `controller-runtime v0.17` · `client-go v0.29` · `Helm SDK v3.14` · `Prometheus client_golang v1.18` · `zap`

## 构建

```bash
make build     # Docker 镜像
make push      # 推送
```

## 设计理念

- **Informer 当闹钟，API 当真相源**——缓存不可信，推送前必须实时验证
- **从监控到自愈**——Operator 不应只是旁观者，应在规则触发时自动执行回滚等修复动作
- **增量告警，非全量轰炸**——Per-release 过滤规则从源头减少噪音
- **一个 Informer 盯所有 Pod**——路由在消费端完成，不浪费集群资源
- **成功不轮询，失败退避停**——节省 API 调用，不给 Helm revision 制造垃圾