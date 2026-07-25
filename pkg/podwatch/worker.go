package podwatch

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/gfhw/inferguard/pkg/log"
	"github.com/gfhw/inferguard/pkg/policy"
)

type WorkerPool struct {
	queue        *PodEventQueue
	k8sClient    kubernetes.Interface
	collector    *PodCollector
	releases     *ReleaseRegistry
	policyEngine *policy.Engine
}

func NewWorkerPool(
	queue *PodEventQueue,
	k8sClient kubernetes.Interface,
	collector *PodCollector,
	releases *ReleaseRegistry,
	policyEngine *policy.Engine,
) *WorkerPool {
	return &WorkerPool{
		queue:        queue,
		k8sClient:    k8sClient,
		collector:    collector,
		releases:     releases,
		policyEngine: policyEngine,
	}
}

func (w *WorkerPool) Start(ctx context.Context) {
	log.Info("WorkerPool started")

	for {
		select {
		case <-ctx.Done():
			log.Info("WorkerPool stopped")
			return
		case <-w.queue.Notify():
			w.drainAll(ctx)
		}
	}
}

func (w *WorkerPool) drainAll(ctx context.Context) {
	for {
		batch := w.queue.DrainAll()
		if len(batch) == 0 {
			return
		}

		log.Info("Processing batch", "batchSize", len(batch), "queueSize", w.queue.Size())

		for _, pod := range batch {
			w.processOnePod(ctx, pod)
		}
	}
}

func (w *WorkerPool) processOnePod(ctx context.Context, pod *PendingPod) {
	releaseCfg := w.releases.Get(pod.ReleaseName)
	if releaseCfg == nil {
		log.Debug("No release config, skipping pod event",
			"release", pod.ReleaseName, "pod", pod.Name)
		return
	}

	// Informer already told us it's deleted 锟?trust it, no API call needed.
	if pod.EventType == PodEventDeleted {
		// Apply per-release event filter.
		if releaseCfg.Filter != nil && !releaseCfg.Filter.ShouldPush(pod.PodInfo, PodEventDeleted) {
			return
		}

		event := &PodEvent{
			Type:        PodEventDeleted,
			OldPod:      pod.Snapshot,
			Namespace:   pod.Namespace,
			ReleaseName: pod.ReleaseName,
			Timestamp:   time.Now().Unix(),
		}
		w.pushAndUpdateMetrics(ctx, releaseCfg, event)
		w.updateCRStatus(ctx, releaseCfg, pod.ReleaseName, pod.Snapshot, PodEventDeleted)
		return
	}

	// Fetch real-time state from API server.
	livePod, err := w.k8sClient.CoreV1().Pods(pod.Namespace).
		Get(ctx, pod.Name, metav1.GetOptions{})

	if err != nil {
		if errors.IsNotFound(err) {
			event := &PodEvent{
				Type:        PodEventDeleted,
				OldPod:      pod.Snapshot,
				Namespace:   pod.Namespace,
				ReleaseName: pod.ReleaseName,
				Timestamp:   time.Now().Unix(),
			}
			log.Info("Pod not found, pushing DELETED",
				"uid", pod.UID, "name", pod.Name,
				"originalEvent", pod.EventType)
			w.pushAndUpdateMetrics(ctx, releaseCfg, event)
			w.updateCRStatus(ctx, releaseCfg, pod.ReleaseName, pod.Snapshot, PodEventDeleted)
			return
		}
		log.ErrorE(err, "Failed to get pod from API, will retry on next resync",
			"uid", pod.UID, "name", pod.Name)
		return
	}

	// Pod exists 锟?push event with API's current state.
	liveInfo := ConvertToPodInfo(livePod)

	// Apply per-release event filter.
	if releaseCfg.Filter != nil && !releaseCfg.Filter.ShouldPush(liveInfo, pod.EventType) {
		return
	}

	event := &PodEvent{
		Type:        pod.EventType,
		Pod:         liveInfo,
		Namespace:   pod.Namespace,
		ReleaseName: pod.ReleaseName,
		Timestamp:   time.Now().Unix(),
	}

	log.Info("Building pod event",
		"type", event.Type,
		"uid", pod.UID,
		"name", pod.Name,
		"phase", liveInfo.Phase)

	w.pushAndUpdateMetrics(ctx, releaseCfg, event)
	w.updateCRStatus(ctx, releaseCfg, pod.ReleaseName, liveInfo, event.Type)

	// Policy engine: evaluate alerting rules.
	if w.policyEngine != nil {
		results := w.policyEngine.Evaluate(ctx, pod.ReleaseName, pod.Namespace,
			policy.PodState{
				Namespace: liveInfo.Namespace,
				Name:      liveInfo.Name,
				Phase:     liveInfo.Phase,
				Ready:     liveInfo.Ready,
				Restart:   liveInfo.Restart,
			}, releaseCfg.Policies)

		// Push Notify results via EventSender (raw user-defined JSON).
		for _, r := range results {
			if releaseCfg.EventSender != nil {
				body := expandAlertVars(r.AlertBody, liveInfo, pod.ReleaseName, nil)
				if err := releaseCfg.EventSender.SendRaw(ctx, body); err != nil {
					log.ErrorE(err, "Failed to push policy alert",
						"policy", r.PolicyName, "release", pod.ReleaseName)
				}
			}
		}
	}
}

func (w *WorkerPool) pushAndUpdateMetrics(ctx context.Context, releaseCfg *ReleaseConfig, event *PodEvent) {
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if releaseCfg.EventSender != nil {
		if err := releaseCfg.EventSender.Send(pushCtx, *event); err != nil {
			podName := event.Pod.Name
			if podName == "" {
				podName = event.OldPod.Name
			}
			log.ErrorE(err, "Failed to push pod event, will retry on next resync",
				"eventType", event.Type,
				"release", event.ReleaseName,
				"pod", podName)
		}
	}

}

func (w *WorkerPool) updateCRStatus(ctx context.Context, releaseCfg *ReleaseConfig, releaseName string, info PodInfo, eventType PodEventType) {
	if releaseCfg.StatusUpdater == nil {
		return
	}

	statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := releaseCfg.StatusUpdater.UpdatePodStatus(statusCtx, releaseName, info, eventType); err != nil {
		log.ErrorE(err, "Failed to update CR pod status",
			"pod", info.Name,
			"namespace", info.Namespace)
	}
}

