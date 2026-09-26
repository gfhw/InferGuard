package podwatch

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/gfhw/inferguard/pkg/log"
)

const (
	metricsNamespace            = "inferguard"
	defaultInferenceMetricsPort = 8000
)

// All Prometheus metrics are AI-inference-specific, scraped live from each
// inference engine Pod (vLLM / TGI / SGLang) at Prometheus scrape time.
// Pod-level state (Phase / Ready / Restart) is handled by the CR Status channel;
// Prometheus stays focused on AI infra observability.

var (
	inferenceLatencyDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_latency_seconds"),
		"Per-output-token generation latency sum (divide by _count for avg)",
		[]string{"namespace", "pod", "release", "revision", "engine", "model"},
		nil,
	)
	inferenceLatencyCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_latency_seconds_count"),
		"Per-output-token generation latency count",
		[]string{"namespace", "pod", "release", "revision", "engine", "model"},
		nil,
	)
	inferenceTTFTDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_time_to_first_token_seconds"),
		"Time-to-first-token latency sum",
		[]string{"namespace", "pod", "release", "revision", "engine", "model"},
		nil,
	)
	inferenceTTFTCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_time_to_first_token_seconds_count"),
		"Time-to-first-token latency count",
		[]string{"namespace", "pod", "release", "revision", "engine", "model"},
		nil,
	)
	inferenceRequestsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_total"),
		"Total successful inference requests",
		[]string{"namespace", "pod", "release", "revision", "engine", "model"},
		nil,
	)
	inferenceRunningDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_running"),
		"Currently running inference requests",
		[]string{"namespace", "pod", "release", "revision", "engine"},
		nil,
	)
	inferenceWaitingDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_waiting"),
		"Currently waiting (queued) inference requests",
		[]string{"namespace", "pod", "release", "revision", "engine"},
		nil,
	)
	inferenceTokensDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_tokens_total"),
		"Total prompt + generation tokens processed",
		[]string{"namespace", "pod", "release", "revision", "engine", "kind"},
		nil,
	)
	inferenceGPUCacheDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_gpu_cache_usage_percent"),
		"GPU KV-cache usage percentage",
		[]string{"namespace", "pod", "release", "revision", "engine"},
		nil,
	)
)

// PodCollector implements prometheus.Collector, exposing only AI inference
// metrics. Pod-level state (Phase / Ready / Restart) is handled by the CR
// status channel, not by Prometheus.
type PodCollector struct {
	podInformer cache.SharedIndexInformer
	filter      *Filter
	releases    *ReleaseRegistry
	httpClient  *http.Client
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
		if !releaseCfg.ScrapeMetrics {
			continue
		}

		info := ConvertToPodInfo(pod)
		if !info.Ready || info.PodIP == "" {
			continue
		}

		// Scrape vLLM / TGI / SGLang /metrics endpoint live.
		c.scrapeAndEmit(ch, info, releaseName, releaseCfg.Revision)
	}
}

// scrapeAndEmit fetches /metrics from the inference engine and emits Prometheus
// metrics under the inferguard_ prefix.
func (c *PodCollector) scrapeAndEmit(ch chan<- prometheus.Metric, info PodInfo, releaseName string, revision int) {
	url := fmt.Sprintf("http://%s:%d/metrics", info.PodIP, defaultInferenceMetricsPort)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		log.Debug("Failed to scrape inference metrics",
			"pod", info.Name, "url", url, "error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return
	}

	engine := detectEngine(string(body))
	if engine == "" {
		// Not a recognized inference engine (vLLM / TGI / SGLang); don't emit
		// mislabeled metrics.
		return
	}
	lines := strings.Split(string(body), "\n")

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
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case strings.HasPrefix(metricName, "vllm:time_per_output_token_seconds_count"):
			ch <- prometheus.MustNewConstMetric(
				inferenceLatencyCountDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case strings.HasPrefix(metricName, "vllm:time_to_first_token_seconds_sum"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTTFTDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case strings.HasPrefix(metricName, "vllm:time_to_first_token_seconds_count"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTTFTCountDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case metricName == "vllm:request_success_total" || strings.HasPrefix(metricName, "vllm:request_success_total{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRequestsDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case metricName == "vllm:num_requests_running" || strings.HasPrefix(metricName, "vllm:num_requests_running{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRunningDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine,
			)
		case metricName == "vllm:num_requests_waiting" || strings.HasPrefix(metricName, "vllm:num_requests_waiting{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceWaitingDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine,
			)
		case strings.HasPrefix(metricName, "vllm:prompt_tokens_total"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTokensDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, "prompt",
			)
		case strings.HasPrefix(metricName, "vllm:generation_tokens_total"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTokensDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, "generation",
			)
		case metricName == "vllm:gpu_cache_usage_perc" || strings.HasPrefix(metricName, "vllm:gpu_cache_usage_perc{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceGPUCacheDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine,
			)
		// TGI (Text Generation Inference) metrics
		case strings.HasPrefix(metricName, "tgi_request_duration_seconds_sum"):
			ch <- prometheus.MustNewConstMetric(
				inferenceLatencyDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case strings.HasPrefix(metricName, "tgi_request_duration_seconds_count"):
			ch <- prometheus.MustNewConstMetric(
				inferenceLatencyCountDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case metricName == "tgi_request_success_total" || strings.HasPrefix(metricName, "tgi_request_success_total{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRequestsDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case metricName == "tgi_queue_size" || strings.HasPrefix(metricName, "tgi_queue_size{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceWaitingDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine,
			)
		case metricName == "tgi_batch_current_size" || strings.HasPrefix(metricName, "tgi_batch_current_size{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRunningDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine,
			)
		// SGLang metrics
		case strings.HasPrefix(metricName, "sglang:time_per_output_token_seconds_sum"):
			ch <- prometheus.MustNewConstMetric(
				inferenceLatencyDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case strings.HasPrefix(metricName, "sglang:time_per_output_token_seconds_count"):
			ch <- prometheus.MustNewConstMetric(
				inferenceLatencyCountDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine, lookupLabel(labels, "model_name"),
			)
		case metricName == "sglang:num_requests_running" || strings.HasPrefix(metricName, "sglang:num_requests_running{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRunningDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine,
			)
		case metricName == "sglang:num_requests_waiting" || strings.HasPrefix(metricName, "sglang:num_requests_waiting{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceWaitingDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName, strconv.Itoa(revision), engine,
			)
		}
	}
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

func detectEngine(metricsBody string) string {
	if strings.Contains(metricsBody, "tgi_") {
		return "tgi"
	}
	if strings.Contains(metricsBody, "sglang:") {
		return "sglang"
	}
	if strings.Contains(metricsBody, "vllm:") {
		return "vllm"
	}
	return ""
}
