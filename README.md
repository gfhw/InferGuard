# InferGuard

AI 推理 Pod 全生命周期管理 Operator —— 声明式部署、实时监控告警、Prometheus 指标出口。一个 CRD 管到底。

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/dl/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28+-326CE5?style=flat&logo=kubernetes)](https://kubernetes.io/)
[![Helm](https://img.shields.io/badge/Helm-3.14+-0F1689?style=flat&logo=helm)](https://helm.sh/)

---

## 定位

Kubernetes 上部署 AI 推理模型不仅仅是 helm install。模型 Pod 要经历 GPU 调度、权重下载(5GB+)、显存分配、预热推理——然后 7x24 小时对外服务。任何时候都可能 GPU OOM、推理延迟飙升、或静默退化。

**InferGuard 覆盖 AI 推理 Pod 的完整生命周期：从 Helm 部署到运行时监控再到智能告警。** Helm 的 --wait --atomic 只管「部署成功那一刻」，之后 Pod 崩了 Helm 不管。InferGuard 把 Helm 拉进 Kubernetes 的 Reconcile 循环——让 Helm Release 像 Deployment 一样具备声明式管理能力。

---

## 核心能力

### Helm 声明式生命周期管理

| 操作 | 方式 |
|------|------|
| 部署模型 | 创建 CR -> Operator 执行 helm install |
| 升级 | 修改 values 或 chart.version -> 自动 helm upgrade |
| 回滚 | 设置 targetRevision -> helm rollback，成功后自动对齐 spec(version + values) |
| 卸载 | 删除 CR -> Finalizer 触发 helm uninstall，释放 GPU 资源 |
| 本地 Chart | chart.localPath 直接加载 .tgz 或目录 |
| Atomic 升级 | atomic: true -> 升级失败自动 rollback |

### AI 推理实时监控

- **Prometheus 专做 AI 指标**(推理延迟、TTFT、Token 吞吐、GPU KV-Cache)。Pod 状态(Phase/Ready/Restart)走 Webhook Push 和 CR Status。
- **主动抓取**: Prometheus 每次 scrape 时，Collector 实时 HTTP GET 每个推理 Pod 的 /metrics(vLLM/TGI/SGLang)，零 Informer 缓存依赖。
- **三路输出**: HTTP Webhook Push(告警) + Prometheus Pull(Grafana) + CR Status 回写(kubectl 直接看)。

### 智能策略告警

- **条件检测**: 声明式定义 Pod 状态条件(PodRestart、PodNotReady、PodCrash)和 AI 指标条件(InferenceLatency、GPUCacheUsage、InferenceQueueDepth)
- **触发计数**: 支持 triggerCount，条件连续满足 N 次才告警，避免抖动
- **自定义告警体**: alertBody 支持占位符 ${release_name} ${pod_name} ${namespace} ${pod_phase} ${pod_restart} ${pod_ip} ${inference_latency_ms} ${gpu_cache_pct}
- **设计理念**: 策略只做告警推送，不做自动回滚——生产环境下机器决策风险太高，人工介入是最佳实践

### Per-release 精细化控制

- **事件过滤器**: onUnhealthyOnly / minRestartCount / ignoreEventTypes 按 release 独立配置
- **独立 Webhook**: 每个 ModelRelease 可推送到不同的 URL，带自定义 headers
- **独立策略**: 每个 release 可配不同的告警规则

---

## 架构

```
ModelRelease CR ---> Controller (Reconcile)
                       +-- Helm SDK: install/upgrade/rollback/uninstall
                       +-- 错误分类: permanent vs transient
                       +-- 指数退避重试(最多10次)

SharedInformer (watch Helm Pods)
  +-- PodEventQueue (UID 去重, 非阻塞通知)
      +-- WorkerPool (单 Worker 串行)
          +-- API Server 实时 Get(非 Informer 缓存)
          +-- Webhook Push(PodEvent JSON + 策略告警)
          +-- CR Status 回写
          +-- Policy Engine -> 条件计数 + 告警推送

Prometheus Collector (主动 scrape, 不依赖 Informer 缓存)
  +-- HTTP GET podIP:8000/metrics (vLLM/TGI/SGLang)
      +-- 解析 vLLM 指标 -> 以 inferguard_ 前缀重新暴露
          +-- 同时评估 AI 策略(InferenceLatency 等)
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
  # wait: false  # 默认 true，设为 false 不等待 Pod 就绪
  podMonitor:
    enabled: true
    endpoint: "https://alerts.example.com/webhook"
    filter:
      onUnhealthyOnly: true
      minRestartCount: 1
  policies:
    - name: high-latency-alert
      condition:
        type: InferenceLatency
        threshold: 5000
      action:
        triggerCount: 3
        alertBody: |
          {
            "release": "${release_name}",
            "pod": "${pod_name}",
            "latency_ms": ${inference_latency_ms},
            "severity": "warning"
          }
    - name: pod-crash-alert
      condition:
        type: PodCrash
      action:
        triggerCount: 1
        alertBody: |
          {
            "release": "${release_name}",
            "pod": "${pod_name}",
            "phase": "${pod_phase}",
            "severity": "critical"
          }
```

---

## Prometheus 指标

Prometheus 只暴露 AI 推理指标，Pod 状态指标请走 Webhook Push 或 CR Status 通道。

| 指标 | Labels | 说明 |
|------|--------|------|
| inferguard_inference_latency_seconds | namespace, pod, release, revision, model | 逐 token 生成延迟(sum) |
| inferguard_inference_latency_seconds_count | namespace, pod, release, revision, model | 逐 token 生成延迟(count) |
| inferguard_inference_time_to_first_token_seconds | namespace, pod, release, revision, model | TTFT 首 token 延迟(sum) |
| inferguard_inference_time_to_first_token_seconds_count | namespace, pod, release, revision, model | TTFT 首 token 延迟(count) |
| inferguard_inference_requests_total | namespace, pod, release, revision, model | 成功请求总数 |
| inferguard_inference_requests_running | namespace, pod, release, revision | 当前正在处理的请求数 |
| inferguard_inference_requests_waiting | namespace, pod, release, revision | 排队等待的请求数 |
| inferguard_inference_tokens_total | namespace, pod, release, revision, kind | Token 总数(kind=prompt|generation) |
| inferguard_inference_gpu_cache_usage_percent | namespace, pod, release, revision | GPU KV-Cache 使用率 |

**工作原理**: Prometheus scrape /metrics -> InferGuard Collector 对每个已注册的推理 Pod 执行 HTTP GET podIP:8000/metrics -> 解析 vLLM 原生指标 -> 以 inferguard_ 前缀重新暴露。零额外 exporter，零 Informer 缓存依赖。

---

## Push 事件格式

```json
{
  "type": "MODIFIED",
  "pod": {
    "namespace": "production",
    "name": "llama-3-8b-abc123",
    "phase": "Running",
    "ready": true,
    "restart": 0
  },
  "oldPod": {"phase": "Pending", "ready": false, "restart": 0},
  "releaseName": "llama-3-8b",
  "timestamp": 1720456789
}
```

策略告警使用用户自定义 JSON 格式(通过 alertBody 定义)，支持占位符变量替换。

---

## 设计决策

| 决策 | 理由 |
|------|------|
| **UID 去重队列** | 比时间窗口去重更简单、准确、CrashLoopBackOff 天然免疫 |
| **API Server 实时 Get(非 Informer 缓存)** | 拒绝可能过期的缓存数据，每次事件都从 API Server 获取当前状态 |
| **单 Worker 串行消费** | 无需锁，无并发竞争，天然有序 |
| **Prometheus 主动 scrape** | 不依赖 Informer 缓存，直接从推理引擎抓取指标 |
| **策略只告警不自愈** | 生产环境机器决策风险太高，人决策更安全。告警推送 + 人工介入是最佳实践 |
| **Prometheus 指标注入 Helm revision** | 业界首创将 Helm revision 作为 Prometheus label，Grafana 可按版本分组对比延迟/吞吐，升级后性能劣化一目了然 |
| **回滚后全量对齐 spec** | 回滚后同步 chart.version + values + ObservedGeneration，避免 spec 与实际状态脱节 |

---

## 快速开始

```bash
# 1. 安装 CRD
make install

# 2. 启动 operator(本地开发)
make run

# 3. 创建 ModelRelease
kubectl apply -f config/samples/inferguard_v1alpha1_modelrelease.yaml

# 4. 查看状态
kubectl get modelrelease -n production
kubectl describe modelrelease llama-3-8b -n production

# 5. 查看 Prometheus 指标
curl http://localhost:9090/metrics | grep inferguard_
```

---

## 项目结构

```
+-- api/v1alpha1/             # CRD 定义(ModelRelease)
+-- cmd/manager/              # Operator 入口
+-- config/samples/           # CR 示例
+-- internal/
|   +-- controller/           # Reconcile 控制器(Helm 生命周期)
|   +-- helm/                 # Helm SDK 封装
+-- pkg/
    +-- podwatch/             # Pod 监控子系统
    |   +-- watcher.go        # SharedInformer + 事件路由
    |   +-- queue.go          # UID 去重队列
    |   +-- worker.go         # 单 Worker 串行处理
    |   +-- collector.go      # Prometheus Collector + AI 指标采集
    |   +-- sender.go         # HTTP Push 事件发送
    |   +-- alert.go          # 告警占位符变量替换
    |   +-- types.go          # 核心数据结构
    +-- policy/               # 策略引擎(条件检测 + 告警)
    |   +-- engine.go
    +-- log/                  # 日志工具
```

---

## 依赖

- Go 1.21+
- Kubernetes 1.28+
- Helm 3.14+

推理引擎 Prometheus 指标兼容性:
- vLLM: 全支持（9 项指标）
- TGI (Text Generation Inference): 支持请求延迟/成功率/队列深度/批处理大小
- SGLang: 支持推理延迟/等待队列/运行请求数
