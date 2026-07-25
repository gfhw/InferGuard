package podwatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/gfhw/inferguard/pkg/log"
	"github.com/gfhw/inferguard/pkg/policy"
)

const (
	metricsNamespace            = "inferguard"
	defaultInferenceMetricsPort = 8000
)

// All Prometheus metrics are AI-inference-specific, scraped live from each
// inference engine Pod (vLLM / TGI / SGLang) at Prometheus scrape time.
// Pod-level state (Phase / Ready / Restart) is handled by the Webhook push
// and CR Status channels ? Prometheus stays focused on AI infra observability.

var (
	inferenceLatencyDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_latency_seconds"),
		"Per-output-token generation latency sum (divide by _count for avg)",
		[]string{"namespace", "pod", "release", "revision", "model"},
		nil,
	)
	inferenceLatencyCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_latency_seconds_count"),
		"Per-output-token generation latency count",
		[]string{"namespace", "pod", "release", "revision", "model"},
		nil,
	)
	inferenceTTFTDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_time_to_first_token_seconds"),
		"Time-to-first-token latency sum",
		[]string{"namespace", "pod", "release", "revision", "model"},
		nil,
	)
	inferenceTTFTCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_time_to_first_token_seconds_count"),
		"Time-to-first-token latency count",
		[]string{"namespace", "pod", "release", "revision", "model"},
		nil,
	)
	inferenceRequestsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_total"),
		"Total successful inference requests",
		[]string{"namespace", "pod", "release", "revision", "model"},
		nil,
	)
	inferenceRunningDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_running"),
		"Currently running inference requests",
		[]string{"namespace", "pod", "release", "revision"},
		nil,
	)
	inferenceWaitingDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_waiting"),
		"Currently waiting (queued) inference requests",
		[]string{"namespace", "pod", "release", "revision"},
		nil,
	)
	inferenceTokensDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_tokens_total"),
		"Total prompt + generation tokens processed",
		[]string{"namespace", "pod", "release", "revision", "kind"},
		nil,
	)
	inferenceGPUCacheDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_gpu_cache_usage_percent"),
		"GPU KV-cache usage percentage",
		[]string{"namespace", "pod", "release", "revision"},
		nil,
	)
)

// PodCollector implements prometheus.Collector, exposing only AI inference metrics.
// Pod-level state (Phase / Ready / Restart) is intentionally excluded ? use the
// Webhook push or CR Status channels for those.
type PodCollector struct {
	podInformer  cache.SharedIndexInformer
	filter       *Filter
	releases     *ReleaseRegistry
	httpClient   *http.Client
	policyEngine *policy.Engine

	eventsProcessed int64
}

func NewPodCollector(podInformer cache.SharedIndexInformer, filter *Filter, releases *ReleaseRegistry) *PodCollector {
	return &PodCollector{
		podInformer: podInformer,
		filter:      filter,
		releases:    releases,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// SetPolicyEngine injects the policy engine for AI-metrics-based auto-remediation.
func (c *PodCollector) SetPolicyEngine(engine *policy.Engine) {
	c.policyEngine = engine
}

func (c *PodCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- inferenceLatencyDesc
	ch <- inferenceLatencyCountDesc
	ch <- inferenceTTFTDesc
	ch <- inferenceTTFTCountDesc
	ch <- inferenceRequestsDesc
	ch <- inferenceRunningDesc
	ch <- inferenceWaitingDesc
	ch <- inferenceTokensDesc
	ch <- inferenceGPUCacheDesc
}

func (c *PodCollector) Collect(ch chan<- prometheus.Metric) {
	releases := c.releases.GetRegisteredReleases()
	tracked := make(map[string]bool, len(releases))
	for name := range releases {
		tracked[name] = true
	}

	objs := c.podInformer.GetStore().List()
	for _, obj := range objs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		if !c.filter.ShouldMonitor(pod) {
			continue
		}

		releaseName := GetReleaseName(pod)
		if releaseName == "" || !tracked[releaseName] {
			continue
		}

		releaseCfg := releases[releaseName]
		if releaseCfg == nil {
			continue
		}

		info := ConvertToPodInfo(pod)
		if !info.Ready || info.PodIP == "" {
			continue
		}

		// Scrape vLLM / TGI / SGLang /metrics endpoint live.
		inferenceState := c.scrapeAndEmit(ch, info, releaseName, releaseCfg.Revision)
		c.eventsProcessed++

		// Evaluate AI-metrics-based policies (e.g. InferenceLatency > threshold -> notify)
		if c.policyEngine != nil && inferenceState != nil {
				results := c.policyEngine.EvaluateInference(context.TODO(),
					releaseName, info.Namespace, *inferenceState, releaseCfg.Policies)

				for _, r := range results {
					if releaseCfg.EventSender != nil {
						body := expandAlertVars(r.AlertBody, info, releaseName, inferenceState)
						_ = releaseCfg.EventSender.SendRaw(context.TODO(), body)
					}
			}
		}
	}
}

// scrapeAndEmit fetches /metrics from the inference engine and emits Prometheus
// metrics under the inferguard_ prefix.
func (c *PodCollector) scrapeAndEmit(ch chan<- prometheus.Metric, info PodInfo, releaseName string, revision int) *policy.InferenceState {
	url := fmt.Sprintf("http://%s:%d/metrics", info.PodIP, defaultInferenceMetricsPort)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		log.Debug("Failed to scrape inference metrics",
			"pod", info.Name, "url", url, "error", err.Error())
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}

	lines := strings.Split(string(body), "\n")
    
    state := &policy.InferenceState{
    	Namespace: info.Namespace,
    	Name:      info.Name,
    }
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		metricName := parts[0]
		val, err := strconv.ParseFloat(parts[len(parts)-1], 64)
		if err != nil {
			continue
		}

		labels := extractLabels(line)

		switch {
		case strings.HasPrefix(metricName, "vllm:time_per_output_token_seconds_sum"):
			ch <- prometheus.MustNewConstMetric(
				inferenceLatencyDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), lookupLabel(labels, "model_name"),
			)
			state.LatencySumSec = val
		case strings.HasPrefix(metricName, "vllm:time_per_output_token_seconds_count"):
			ch <- prometheus.MustNewConstMetric(
				inferenceLatencyCountDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), lookupLabel(labels, "model_name"),
			)
			state.LatencyCount = val
		case strings.HasPrefix(metricName, "vllm:time_to_first_token_seconds_sum"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTTFTDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), lookupLabel(labels, "model_name"),
			)
		case strings.HasPrefix(metricName, "vllm:time_to_first_token_seconds_count"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTTFTCountDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), lookupLabel(labels, "model_name"),
			)
		case metricName == "vllm:request_success_total" || strings.HasPrefix(metricName, "vllm:request_success_total{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRequestsDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), lookupLabel(labels, "model_name"),
			)
		case metricName == "vllm:num_requests_running" || strings.HasPrefix(metricName, "vllm:num_requests_running{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRunningDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision),
			)
		case metricName == "vllm:num_requests_waiting" || strings.HasPrefix(metricName, "vllm:num_requests_waiting{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceWaitingDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision),
			)
			state.RequestsWaiting = int32(val)
		case strings.HasPrefix(metricName, "vllm:prompt_tokens_total"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTokensDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), "prompt",
			)
		case strings.HasPrefix(metricName, "vllm:generation_tokens_total"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTokensDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), "generation",
			)
		case metricName == "vllm:gpu_cache_usage_perc" || strings.HasPrefix(metricName, "vllm:gpu_cache_usage_perc{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceGPUCacheDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision),
			)
			state.GPUCachePct = val
		}
	}
	return state
}

func extractLabels(line string) map[string]string {
	labels := make(map[string]string)
	start := strings.Index(line, "{")
	end := strings.Index(line, "}")
	if start < 0 || end <= start {
		return labels
	}
	pairs := strings.Split(line[start+1:end], ",")
	for _, pair := range pairs {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) != 2 {
			continue
		}
		labels[kv[0]] = strings.Trim(kv[1], "\"")
	}
	return labels
}

func lookupLabel(labels map[string]string, key string) string {
	if v, ok := labels[key]; ok {
		return v
	}
	return ""
}

// StartPrometheusServer registers the collector and serves /metrics.
func StartPrometheusServer(ctx context.Context, addr string, collector *PodCollector) error {
	registry := prometheus.NewRegistry()
	if err := registry.Register(collector); err != nil {
		return fmt.Errorf("failed to register pod collector: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Info("Starting Prometheus metrics server (AI inference only)", "addr", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("prometheus server error: %w", err)
	}
	return nil
}


