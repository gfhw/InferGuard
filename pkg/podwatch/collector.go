package podwatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/gfhw/inferguard/pkg/log"
)

const (
	metricsNamespace          = "inferguard"
	defaultInferenceMetricsPort = 8000
)

// ??? Pod-level metrics (source: Informer) ???

var (
	podInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "pod_info"),
		"Information about managed pods",
		[]string{"namespace", "name", "release", "phase", "node", "pod_ip"},
		nil,
	)
	podReadyDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "pod_ready"),
		"Whether the managed pod is ready (1=ready, 0=not ready)",
		[]string{"namespace", "name", "release"},
		nil,
	)
	podRestartDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "pod_restart_total"),
		"Total restart count of the managed pod",
		[]string{"namespace", "name", "release"},
		nil,
	)
	podEventsTotalDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "pod_events_total"),
		"Total number of pod events processed by type",
		[]string{"event_type"},
		nil,
	)
)

// ??? AI inference metrics (source: active scrape of vLLM/TGI /metrics) ???

var (
	inferenceLatencyDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_latency_seconds"),
		"Per-output-token generation latency in seconds (sum/count for avg)",
		[]string{"namespace", "pod", "release", "model"},
		nil,
	)
	inferenceTTFTDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_time_to_first_token_seconds"),
		"Time-to-first-token latency in seconds (sum/count for avg)",
		[]string{"namespace", "pod", "release", "model"},
		nil,
	)
	inferenceRequestsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_total"),
		"Total successful inference requests",
		[]string{"namespace", "pod", "release", "model"},
		nil,
	)
	inferenceRunningDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_running"),
		"Currently running inference requests",
		[]string{"namespace", "pod", "release"},
		nil,
	)
	inferenceWaitingDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_requests_waiting"),
		"Currently waiting inference requests (queued)",
		[]string{"namespace", "pod", "release"},
		nil,
	)
	inferenceTokensDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_tokens_total"),
		"Total prompt + generation tokens processed",
		[]string{"namespace", "pod", "release", "kind"},
		nil,
	)
	inferenceGPUCacheDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "inference_gpu_cache_usage_percent"),
		"GPU KV cache usage percentage",
		[]string{"namespace", "pod", "release"},
		nil,
	)
)

// PodCollector implements prometheus.Collector.
// Pod-level metrics come from the Informer cache.
// AI inference metrics are scraped live from each inference pod's /metrics endpoint.
type PodCollector struct {
	podInformer cache.SharedIndexInformer
	filter      *Filter
	releases    *ReleaseRegistry
	httpClient  *http.Client

	eventsMu       sync.Mutex
	eventsAdded    int64
	eventsModified int64
	eventsDeleted  int64
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
	ch <- podInfoDesc
	ch <- podReadyDesc
	ch <- podRestartDesc
	ch <- podEventsTotalDesc
	ch <- inferenceLatencyDesc
	ch <- inferenceTTFTDesc
	ch <- inferenceRequestsDesc
	ch <- inferenceRunningDesc
	ch <- inferenceWaitingDesc
	ch <- inferenceTokensDesc
	ch <- inferenceGPUCacheDesc
}

func (c *PodCollector) Collect(ch chan<- prometheus.Metric) {
	objs := c.podInformer.GetStore().List()
	releases := c.releases.GetRegisteredReleases()

	// Build a set of release names we care about.
	tracked := make(map[string]bool, len(releases))
	for name := range releases {
		tracked[name] = true
	}

	for _, obj := range objs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		if !c.filter.ShouldMonitor(pod) {
			continue
		}

		info := ConvertToPodInfo(pod)
		releaseName := GetReleaseName(pod)
		if releaseName == "" {
			continue
		}
		ready := float64(0)
		if info.Ready {
			ready = 1
		}

		// ?? Pod-level metrics (always emitted) ??
		ch <- prometheus.MustNewConstMetric(
			podInfoDesc, prometheus.GaugeValue, 1,
			info.Namespace, info.Name, releaseName, info.Phase, info.NodeName, info.PodIP,
		)
		ch <- prometheus.MustNewConstMetric(
			podReadyDesc, prometheus.GaugeValue, ready,
			info.Namespace, info.Name, releaseName,
		)
		ch <- prometheus.MustNewConstMetric(
			podRestartDesc, prometheus.GaugeValue, float64(info.Restart),
			info.Namespace, info.Name, releaseName,
		)

		// ?? AI inference metrics (active scrape) ??
		// Only scrape pods that belong to a registered release and have a PodIP.
		if tracked[releaseName] && info.Ready && info.PodIP != "" {
			c.scrapeAndEmitInferenceMetrics(ch, info, releaseName)
		}
	}

	// ?? Event counters ??
	c.eventsMu.Lock()
	ch <- prometheus.MustNewConstMetric(podEventsTotalDesc, prometheus.CounterValue, float64(c.eventsAdded), "added")
	ch <- prometheus.MustNewConstMetric(podEventsTotalDesc, prometheus.CounterValue, float64(c.eventsModified), "modified")
	ch <- prometheus.MustNewConstMetric(podEventsTotalDesc, prometheus.CounterValue, float64(c.eventsDeleted), "deleted")
	c.eventsMu.Unlock()
}

// scrapeAndEmitInferenceMetrics fetches /metrics from the inference engine
// (vLLM, TGI, etc.) and emits Prometheus metrics under the inferguard_ prefix.
func (c *PodCollector) scrapeAndEmitInferenceMetrics(ch chan<- prometheus.Metric, info PodInfo, releaseName string) {
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB max
	if err != nil {
		return
	}

	// Parse vLLM-style Prometheus metrics (simple line-based parsing).
	// We look for well-known metric names and extract their values.
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
				info.Namespace, info.Name, releaseName, lookupLabel(labels, "model_name"),
			)
		case strings.HasPrefix(metricName, "vllm:time_to_first_token_seconds_sum"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTTFTDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, lookupLabel(labels, "model_name"),
			)
		case metricName == "vllm:request_success_total" || strings.HasPrefix(metricName, "vllm:request_success_total{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRequestsDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, lookupLabel(labels, "model_name"),
			)
		case metricName == "vllm:num_requests_running" || strings.HasPrefix(metricName, "vllm:num_requests_running{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceRunningDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName,
			)
		case metricName == "vllm:num_requests_waiting" || strings.HasPrefix(metricName, "vllm:num_requests_waiting{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceWaitingDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName,
			)
		case strings.HasPrefix(metricName, "vllm:prompt_tokens_total"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTokensDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, "prompt",
			)
		case strings.HasPrefix(metricName, "vllm:generation_tokens_total"):
			ch <- prometheus.MustNewConstMetric(
				inferenceTokensDesc, prometheus.CounterValue, val,
				info.Namespace, info.Name, releaseName, "generation",
			)
		case metricName == "vllm:gpu_cache_usage_perc" || strings.HasPrefix(metricName, "vllm:gpu_cache_usage_perc{"):
			ch <- prometheus.MustNewConstMetric(
				inferenceGPUCacheDesc, prometheus.GaugeValue, val,
				info.Namespace, info.Name, releaseName,
			)
		}
	}
}

// extractLabels parses Prometheus metric labels from a line like:
//   metric_name{key1="val1",key2="val2"} value
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

// lookupLabel returns the label value or empty string.
func lookupLabel(labels map[string]string, key string) string {
	if v, ok := labels[key]; ok {
		return v
	}
	return ""
}

func (c *PodCollector) RecordEvent(eventType PodEventType) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()

	switch eventType {
	case PodEventAdded:
		c.eventsAdded++
	case PodEventModified:
		c.eventsModified++
	case PodEventDeleted:
		c.eventsDeleted++
	}
}

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

	log.Info("Starting Prometheus metrics server", "addr", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("prometheus server error: %w", err)
	}
	return nil
}
