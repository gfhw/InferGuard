# InferGuard

面向 AI 推理 Pod 的 Kubernetes Operator —— 声明式 Helm 生命周期管理 + 推理指标版本画像 + 部署后验证闭环。

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/dl/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28+-326CE5?style=flat&logo=kubernetes)](https://kubernetes.io/)
[![Helm](https://img.shields.io/badge/Helm-3.14+-0F1689?style=flat&logo=helm)](https://helm.sh/)

---

## 定位

Kubernetes 上部署 AI 推理模型不仅仅是 `helm install`。模型 Pod 要经历 GPU 调度、权重下载(5GB+)、显存分配、预热推理——然后 7x24 小时对外服务。任何时候都可能 GPU OOM、推理延迟飙升、或静默退化。

Helm 的 `--wait --atomic` 只管「Pod Ready 那一刻」,但 **Pod Ready ≠ 模型真的能推理**——探针只能回答"进程还活着吗"(布尔),回答不了"推理质量达标吗"(定量 SLA)。

**InferGuard 聚焦的正是这个缝隙**:把 Helm 拉进 Kubernetes 的 Reconcile 循环,让 Helm Release 像 Deployment 一样具备声明式管理能力;同时把**部署版本(revision)**与**推理指标**、**验证结果**关联起来,回答"这次升级有没有劣化"。

---

## 核心能力

### 1. Helm 声明式生命周期管理

| 操作 | 方式 |
|------|------|
| 部署模型 | 创建 CR -> Operator 执行 helm install |
| 升级 | 修改 values 或 chart.version -> 自动 helm upgrade |
| 回滚 | 设置 targetRevision -> helm rollback,成功后自动对齐 spec(version + values) |
| 卸载 | 删除 CR -> Finalizer 触发 helm uninstall,释放 GPU 资源 |
| 本地 Chart | chart.localPath 直接加载 .tgz 或目录 |
| Atomic 升级 | atomic: true -> 升级失败自动 rollback |

内置错误分类(permanent vs transient)、指数退避重试(最多 10 次)、generation 感知的 retry 幂等(spec 变化自动重置重试)。

### 2. Pod 状态自描述

`SharedInformer` 监听 Helm 管理的 Pod,通过 **UID 去重队列(单 map + 类型升级合并)** 单 worker 串行消费,实时 Get 后回写到 CR 的 `status.podStatuses`。

`kubectl describe modelrelease` 能直接看到每个 Pod 的 phase/ready/restart,以及 **OOMKilled / 退出原因 / reason / message** 等 AI 运维诊断信息。

### 3. AI 指标采集(带 revision 版本画像)

Prometheus 每次 scrape 时,Collector **现场** HTTP GET 每个推理 Pod 的 `:8000/metrics`,解析 vLLM/TGI/SGLang 原生指标,归一化后以 `inferguard_` 前缀重新暴露。

关键差异:**所有指标注入 `release` + `revision` label**。vLLM 自己不知道"我属于哪个 Helm release、是第几次部署",而 InferGuard 管生命周期、恰好知道。这让 Grafana 能按部署版本分组对比延迟/吞吐——升级后性能劣化一目了然。

> **告警交给谁**:运行期的持续阈值监控交给 Prometheus alerting rules + Alertmanager(支持 `for` 抖动免疫、分组、静默、路由)。InferGuard 只负责把 AI 指标**采集出来并关联 revision**,不抢告警的活。

---

## 规划中的能力

### 部署后验证闭环(robot operator)

> 字段与状态机已定义,逻辑待接线(配合独立的 robot operator)。

```
helm 部署成功 → 创建 InferenceCheck CR → robot operator 起 Job 跑 Robot 断言
→ 回写 verified/degraded → InferGuard watch 并写回 status.verification
```

`status.verification.phase` 独立于 `status.phase`(后者只表达 Helm 生命周期),取值:

| phase | 含义 |
|-------|------|
| Skipped | 未启用验证(部署即视为可用) |
| Pending | 验证 CR 已创建,用例未跑完 |
| Verified | 全部断言通过 |
| Degraded | 有断言失败 |
| Unknown | 验证出错/超时,无法判定 |

**分层定位**:探针管"就绪"(进程/服务活着),robot 验证管"质量验收"(TTFT/吞吐/KV-cache 定量断言 + 升级回归),两者互补。

### 火山调度委托

`spec.scheduling` 声明 AI Pod 的调度意图(`schedulerName: volcano` + `podGroup` 的 gang 调度配置)。**InferGuard 只声明意图、创建 PodGroup 委托给 Volcano,不实现调度算法**——调度是集群控制平面的职责,不是应用控制器的职责。

---

## 架构

```
ModelRelease CR ---> Controller (Reconcile)
                       +-- Helm SDK: install/upgrade/rollback/uninstall
                       +-- 错误分类 + 指数退避 + generation 感知 retry
                       +-- 声明调度意图(scheduling)/ 验证意图(verification)

SharedInformer (watch Helm Pods)
  +-- PodEventQueue(UID 去重 + 类型升级合并)
      +-- WorkerPool(单 Worker 串行)
          +-- API Server 实时 Get(非 Informer 缓存)
          +-- CR Status 回写(含 OOM/退出原因)

Prometheus Collector(被动两段 pull)
  +-- HTTP GET podIP:8000/metrics(vLLM/TGI/SGLang)
      +-- 归一化 -> inferguard_ 前缀 + release/revision label
```

---

## CR 示例

```yaml
apiVersion: inferguard.io/v1alpha1
kind: ModelRelease
metadata:
  name: llama-3-8b
  namespace: production
spec:
  chart:
    repository: https://charts.example.com/
    name: vllm
    version: "0.5.0"
    # localPath: ./charts/vllm-0.5.0.tgz  # 本地 chart 二选一
  values:
    model: "meta-llama/Meta-Llama-3-8B-Instruct"
    replicas: 2
  # atomic: true  # 升级失败自动回滚
  # wait: false  # 默认 true,设为 false 不等待 Pod 就绪

  # 调度意图:委托 Volcano 做 gang/队列调度(不实现调度)
  scheduling:
    schedulerName: volcano
    podGroup:
      minMember: 2
      queue: default

  # 部署后验证:跑 robot 断言,结果回写 status.verification
  verification:
    enabled: true
    suites: [smoke, regression]

  podMonitor:
    enabled: true
```

---

## Prometheus 指标

只暴露 AI 推理指标(带 `namespace` / `pod` / `release` / `revision` / `engine` / `model` label)。

| 指标 | 说明 |
|------|------|
| inferguard_inference_latency_seconds{,_count} | 逐 token 生成延迟(sum/count) |
| inferguard_inference_time_to_first_token_seconds{,_count} | TTFT 首 token 延迟(sum/count) |
| inferguard_inference_requests_total | 成功请求总数 |
| inferguard_inference_requests_running / _waiting | 运行中 / 排队请求数 |
| inferguard_inference_tokens_total | Token 总数(kind=prompt\|generation) |
| inferguard_inference_gpu_cache_usage_percent | GPU KV-Cache 使用率 |

**工作原理**:Prometheus scrape /metrics -> Collector 对每个已注册的推理 Pod 执行 HTTP GET `podIP:8000/metrics` -> 解析 vLLM/TGI/SGLang 原生指标 -> 以 `inferguard_` 前缀重新暴露并注入 release/revision label。零额外 exporter,零 Informer 缓存依赖。

---

## 设计决策

| 决策 | 理由 |
|------|------|
| **UID 去重 + 类型升级合并** | 单 map 按 UID 去重,`DELETED>MODIFIED>ADDED` 升级合并——既防队列被打爆,又保证事件类型不失真 |
| **API Server 实时 Get(非 Informer 缓存)** | 拒绝可能过期的缓存数据,每次事件都从 API Server 获取当前状态 |
| **单 Worker 串行消费** | 无需锁,无并发竞争,天然有序 |
| **Prometheus 主动 scrape(两段 pull)** | scrape 时现场抓推理引擎,不依赖缓存 |
| **注入 Helm revision 作 Prometheus label** | 按部署版本分组对比延迟/吞吐,升级劣化一目了然 |
| **回滚后全量对齐 spec** | 回滚后同步 chart.version + values,避免 spec 与实际状态脱节 |
| **operator 不抢告警的活** | 通用 Pod 告警交给 kube-state-metrics,持续阈值交给 Alertmanager;operator 聚焦版本画像 + 验证闭环 |
| **调度只声明不实现** | 调度是控制平面职责,operator 声明意图 + 创建 PodGroup 委托 Volcano |
| **探针 vs robot 分层** | 探针判"就绪"(布尔),robot 判"质量验收"(定量 SLA + 回归),两者互补 |

---

## 快速开始

```bash
# 1. 安装 CRD
kubectl apply -f config/crd/bases/inferguard.io_modelreleases.yaml

# 2. 启动 operator(本地开发)
go run ./cmd/manager

# 3. 创建 ModelRelease
kubectl apply -f config/samples/modelrelease_v1alpha1_modelrelease.yaml

# 4. 查看状态
kubectl get modelrelease -n production
kubectl describe modelrelease llama-3-8b -n production

# 5. 查看 Prometheus 指标
curl http://localhost:8080/metrics | grep inferguard_
```

---

## 项目结构

```
+-- api/v1alpha1/             # CRD 定义(ModelRelease + 调度/验证字段)
+-- cmd/manager/              # Operator 入口
+-- config/                   # CRD / RBAC / Prometheus / 样本
+-- internal/
|   +-- controller/           # Reconcile 控制器(Helm 生命周期)
|   +-- helm/                 # Helm SDK 封装
+-- pkg/
    +-- podwatch/             # Pod 监控子系统
    |   +-- watcher.go        # SharedInformer + 事件路由
    |   +-- queue.go          # UID 去重队列(类型升级合并)
    |   +-- worker.go         # 单 Worker 串行,回写 CR 状态
    |   +-- collector.go      # Prometheus Collector + AI 指标采集
    |   +-- types.go          # 核心数据结构
    |   +-- filter.go         # Pod label 过滤
    +-- log/                  # 日志工具
```

---

## 依赖

- Go 1.21+
- Kubernetes 1.28+
- Helm 3.14+

推理引擎 Prometheus 指标兼容性:
- vLLM: 全支持(9 项指标)
- TGI (Text Generation Inference): 支持请求延迟/成功率/队列深度/批处理大小
- SGLang: 支持推理延迟/等待队列/运行请求数
