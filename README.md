# Watchpod

将 Helm 的声明式能力原生融入 Kubernetes Operator 模式——通过一个 CRD 控制 Chart 的全生命周期，部署后实时监控 Pod 健康状态，异常时自动回滚保障业务可用性。

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/dl/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28+-326CE5?style=flat&logo=kubernetes)](https://kubernetes.io/)
[![Helm](https://img.shields.io/badge/Helm-3.14+-0F1689?style=flat&logo=helm)](https://helm.sh/)

---

## 解决了什么问题

Helm 是 Kubernetes 的包管理器——但它工作在 Kubernetes 的声明式模型之外。你跑完 `helm install`，Helm 就退出了。Pod 有没有真的健康运行、升级失败后要不要回滚、回滚完了谁告诉你结果——这些 Helm 都不管。

Watchpod 把 Helm 拉进了 Kubernetes 的 Operator 模型：**Helm Release 不再是 CLI 的一次性操作，而是 CR 的期望状态**。创建 CR → 自动部署，改 values → 自动升级，Pod 崩了 → 自动回滚。从部署、监控到自愈，一个 CRD 全闭环。

---

## 核心能力

### Helm 生命周期声明式管理

| 操作 | 方式 |
|------|------|
| 部署 Chart | 创建 HelmRelease CR → Operator 执行 `helm install` |
| 升级 | 修改 `values` 或 `chart.version` → Operator 执行 `helm upgrade` |
| 回滚 | 设 `targetRevision` → Operator 执行 `helm rollback`，成功后自动对齐 spec |
| 卸载 | 删除 CR → Finalizer 触发 `helm uninstall` 清理全部资源 |
| 本地 Chart | `chart.localPath` 直接加载 `.tgz` 或目录，无需 Helm 仓库 |

### Pod 运行时监控

- **全局单 Informer** watch 所有 Helm 管理的 Pod，零轮询、零遗漏
- **UID 去重削峰**：同一 Pod 瞬间多次事件合并为一条，防止下游轰炸
- **两阶段验证**：Informer 唤醒 + API Server 实时查——推送的是当前真实状态，不是过期缓存
- **三路输出**：HTTP Webhook Push + Prometheus Pull + CR Status 回写

### 智能自愈

- **策略引擎**：声明条件（Pod restart > 5）→ 动作（自动 rollback）。在 Pod 事件处理链路中内嵌评估，触发后复用现有 Helm 操作和 EventSender 通道
- **错误分类重试**：区分 chart 问题（不重试）和网络故障（指数退避，最多 10 次），避免无意义 revision 堆积
- **稳定态不轮询**：成功部署后不再 requeue，只有 spec 变化才触发新的 reconcile

### Per-release 精细化控制

- **事件过滤器**：`onUnhealthyOnly`（只推不健康）、`minRestartCount`（按阈值过滤）、`ignoreEventTypes`（跳过指定类型）
- **独立 Webhook**：每个 HelmRelease 可配不同的推送地址、HTTP 方法、自定义 headers
- **独立策略**：每个 HelmRelease 可配不同的自愈规则

---

## 设计哲学

**Helm 负责"部署"，Watchpod 负责"管到底"。**

Helm 的 `--wait --atomic --timeout` 只管到"部署这个动作成功"，不管部署后 Pod 的实际运行状态。Watchpod 延伸了这条链路：

```
Helm：         install → deployed（结束了）
Watchpod：     install → deployed → 持续 watch Pod → 不健康 → 自动 rollback
```

这不是替代 Helm，而是把 Helm 嵌入 Kubernetes 的 reconcile 循环——让 Helm Release 像 Deployment 一样具备自愈能力。

---

## 架构

```
                        kube-apiserver
                             |
              +--------------+--------------+
              |                             |
    HelmRelease Controller         SharedInformer
    (Reconcile: install/            (watch all Helm Pods)
     upgrade/rollback/                    |
     uninstall)                     PodEventQueue
              |                    (UID dedup)
              |                          |
              |                     WorkerPool
              |                    (serial, lock-free)
              |                          |
              +-----------+--------------+
                          |
              ReleaseRegistry（per-release 路由）
                          |
          +------+--------+--------+------+
          |      |        |        |      |
      EventSender  |  ReleaseFilter  |  CR Status
     (HTTP Push)   |  (CR 可配)     |  (.status)
                   |                |
             Prometheus        PolicyEngine
             (/metrics)        (条件→动作)
                                   |
                             Auto-Rollback
```

**从 Chart 部署到异常自愈，一条链路贯通。**

---

## 业界对比

| | Watchpod | Flux CD | ArgoCD | Robusta | Helm CLI |
|------|:---:|:---:|:---:|:---:|:---:|
| Helm Release CRD | ✅ | ✅ | ✅（Application） | — | — |
| 升级失败自动回滚 | ✅ | ✅ | ✅ | — | `--atomic` |
| Pod 运行时监控 Push | ✅ | — | — | ✅ | — |
| 运行时异常自动回滚 | ✅ | — | — | ✅ | — |
| Per-release 事件过滤 | ✅ | — | — | — | — |
| CR Status 回写 Pod 状态 | ✅ | — | — | — | — |
| 部署复杂度 | 单二进制 | 4+ 组件 | 3+ 组件 | Helm + SaaS | 单 CLI |
| 学习成本 | 低 | 高 | 高 | 中 | 低 |

**差异化定位：** Watchpod 不是 Flux/ArgoCD 的替代品——它不做 GitOps。它填补的是 Helm CLI 和 GitOps 工具之间的空白：以 CR 为操作界面、以 Pod 运行时状态为判断依据、以自动回滚为兜底，轻量、自洽、开箱即用。

---

## HelmRelease CRD

### 完整示例

```yaml
apiVersion: helm.watchpod.io/v1alpha1
kind: HelmRelease
metadata:
  name: my-nginx
spec:
  releaseName: my-nginx
  chart:
    repository: https://charts.bitnami.com/bitnami
    name: nginx
    version: "15.10.3"
    # 本地 Chart：localPath: /charts/nginx-15.10.3.tgz
  values:
    replicaCount: 3
  atomic: true
  waitTimeout: 300
  podMonitor:
    enabled: true
    endpoint: "https://alerts.mycompany.com/webhook"
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
| Chart | `chart.repository` / `chart.name` / `chart.version` | 远程仓库 |
| Chart | `chart.localPath` | 本地 `.tgz` 或目录 |
| Helm | `values` | 透传 Helm `.Values` |
| Helm | `atomic` | 升级失败自动 rollback |
| Helm | `forceUpgrade` | 强制更新不可变资源 |
| Helm | `targetRevision` | 触发回滚（成功后自动清空） |
| Helm | `waitTimeout` | 超时秒数（默认 300） |
| 监控 | `podMonitor.enabled` | 开关 |
| 监控 | `podMonitor.endpoint` | Webhook 地址 |
| 过滤 | `podMonitor.filter.onUnhealthyOnly` | 只推不健康事件 |
| 过滤 | `podMonitor.filter.minRestartCount` | restart 阈值 |
| 过滤 | `podMonitor.filter.ignoreEventTypes` | 跳过的事件类型 |
| 策略 | `policies[].condition.type` | PodRestart / PodNotReady / PodCrash |
| 策略 | `policies[].condition.threshold` | 触发阈值 |
| 策略 | `policies[].action.type` | Rollback / Notify |

---

## 使用方法

```bash
# 部署 Operator
kubectl apply -f charts/watchpod-operator/templates/crd.yaml
kubectl apply -k config/default

# 创建 HelmRelease
kubectl apply -f helmrelease.yaml

# 查看状态
kubectl get helmrelease my-nginx -o yaml
```

---

## 推送事件格式

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
| `pod` | API 实时状态（DELETED 为空） |
| `oldPod` | 仅在 DELETED 出现——Pod 被删前的最后已知状态 |
| `releaseName` | 所属 HelmRelease |

---

## Prometheus 指标

| 指标 | Labels | 说明 |
|------|--------|------|
| `watchpod_pod_info` | namespace, name, release, phase, node, pod_ip | Pod 元数据 |
| `watchpod_pod_ready` | namespace, name, release | 1=Ready, 0=Not |
| `watchpod_pod_restart_total` | namespace, name, release | 累计 restart |
| `watchpod_pod_events_total` | event_type | 已处理事件计数 |

---

## 项目结构

```
api/v1alpha1/              CRD 类型：HelmRelease + 策略/过滤
cmd/manager/main.go        入口，组装全链路
internal/
  controller/              Reconcile 循环 + Helm 操作 + CR Status
  helm/                    Helm v3 SDK（install/upgrade/rollback/uninstall + chart 下载/本地加载）
pkg/
  podwatch/                Pod 监控：watcher、queue、worker、sender、collector、filter
  policy/                  策略引擎：条件匹配 + 动作执行
  log/                     zap 日志
charts/watchpod-operator/  Helm Chart 部署资源
```

## 技术栈

`Go 1.21+` · `controller-runtime v0.17` · `client-go v0.29` · `Helm SDK v3.14` · `Prometheus client_golang v1.18`

---

> **Watchpod：让 Helm Release 像 Deployment 一样自愈。**