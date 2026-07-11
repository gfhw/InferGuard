package podwatch

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"watchpod/pkg/log"
)

const (
	metricsNamespace = "watchpod"
)

var (
	podInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "pod_info"),
		"Information about Helm-managed pods",
		[]string{"namespace", "name", "release", "phase", "node", "pod_ip", "event_type"},
		nil,
	)

	podReadyDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "pod_ready"),
		"Whether the Helm-managed pod is ready (1=ready, 0=not ready)",
		[]string{"namespace", "name", "release"},
		nil,
	)

	podRestartDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", "pod_restart_total"),
		"Total restart count of the Helm-managed pod",
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

type PodCollector struct {
	podInformer cache.SharedIndexInformer
	filter      *Filter

	eventsMu       sync.Mutex
	eventsAdded    int64
	eventsModified int64
	eventsDeleted  int64
}

func NewPodCollector(podInformer cache.SharedIndexInformer, filter *Filter) *PodCollector {
	return &PodCollector{
		podInformer: podInformer,
		filter:      filter,
	}
}

func (c *PodCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- podInfoDesc
	ch <- podReadyDesc
	ch <- podRestartDesc
	ch <- podEventsTotalDesc
}

func (c *PodCollector) Collect(ch chan<- prometheus.Metric) {
	objs := c.podInformer.GetStore().List()

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
		ready := float64(0)
		if info.Ready {
			ready = 1
		}

		ch <- prometheus.MustNewConstMetric(
			podInfoDesc,
			prometheus.GaugeValue,
			1,
			info.Namespace,
			info.Name,
			releaseName,
			info.Phase,
			info.NodeName,
			info.PodIP,
			"",
		)

		ch <- prometheus.MustNewConstMetric(
			podReadyDesc,
			prometheus.GaugeValue,
			ready,
			info.Namespace,
			info.Name,
			releaseName,
		)

		ch <- prometheus.MustNewConstMetric(
			podRestartDesc,
			prometheus.GaugeValue,
			float64(info.Restart),
			info.Namespace,
			info.Name,
			releaseName,
		)
	}

	c.eventsMu.Lock()
	ch <- prometheus.MustNewConstMetric(podEventsTotalDesc, prometheus.CounterValue, float64(c.eventsAdded), "added")
	ch <- prometheus.MustNewConstMetric(podEventsTotalDesc, prometheus.CounterValue, float64(c.eventsModified), "modified")
	ch <- prometheus.MustNewConstMetric(podEventsTotalDesc, prometheus.CounterValue, float64(c.eventsDeleted), "deleted")
	c.eventsMu.Unlock()
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
