package podwatch

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/gfhw/inferguard/pkg/log"
)

type WorkerPool struct {
	queue     *PodEventQueue
	k8sClient kubernetes.Interface
	collector *PodCollector
	releases  *ReleaseRegistry
}

func NewWorkerPool(
	queue *PodEventQueue,
	k8sClient kubernetes.Interface,
	collector *PodCollector,
	releases *ReleaseRegistry,
) *WorkerPool {
	return &WorkerPool{
		queue:     queue,
		k8sClient: k8sClient,
		collector: collector,
		releases:  releases,
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

// processOnePod resolves a queued pod event to the pod's live state and writes
// it back to the owning ModelRelease's status.podStatuses. This is the operator's
// "senses": it keeps the CR's self-description current without pushing a
// duplicated event stream to an external webhook.
func (w *WorkerPool) processOnePod(ctx context.Context, pod *PendingPod) {
	releaseCfg := w.releases.Get(pod.ReleaseName)
	if releaseCfg == nil {
		log.Debug("No release config, skipping pod event",
			"release", pod.ReleaseName, "pod", pod.Name)
		return
	}

	// Informer already told us it's deleted; trust it and remove the pod from
	// the CR using the snapshot captured at enqueue time.
	if pod.EventType == PodEventDeleted {
		w.updateCRStatus(ctx, releaseCfg, pod.ReleaseName, pod.Snapshot, PodEventDeleted)
		return
	}

	// Fetch the pod's live state from the API server (not the informer cache) so
	// the CR never reflects a stale snapshot.
	livePod, err := w.k8sClient.CoreV1().Pods(pod.Namespace).
		Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Pod not found, treating as DELETED",
				"uid", pod.UID, "name", pod.Name,
				"originalEvent", pod.EventType)
			w.updateCRStatus(ctx, releaseCfg, pod.ReleaseName, pod.Snapshot, PodEventDeleted)
			return
		}
		log.ErrorE(err, "Failed to get pod from API, will retry on next resync",
			"uid", pod.UID, "name", pod.Name)
		return
	}

	w.updateCRStatus(ctx, releaseCfg, pod.ReleaseName, ConvertToPodInfo(livePod), pod.EventType)
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
