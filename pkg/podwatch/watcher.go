package podwatch

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/gfhw/inferguard/pkg/log"
	"github.com/gfhw/inferguard/pkg/policy"
)

type GlobalPodWatcher struct {
	k8sClient      kubernetes.Interface
	resyncInterval time.Duration
	prometheusAddr string

	podInformer cache.SharedIndexInformer

	queue *PodEventQueue

	workerPool *WorkerPool
	collector  *PodCollector
	filter     *Filter

	releases      *ReleaseRegistry

	policyEngine  *policy.Engine

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	started bool
}

func NewGlobalPodWatcher(k8sClient kubernetes.Interface, resyncInterval time.Duration, prometheusAddr string) *GlobalPodWatcher {
	return &GlobalPodWatcher{
		k8sClient:      k8sClient,
		resyncInterval: resyncInterval,
		prometheusAddr: prometheusAddr,
		queue:          NewPodEventQueue(),
		filter:         NewFilter(nil),
		releases:       NewReleaseRegistry(),
	}
}

func (w *GlobalPodWatcher) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.started {
		return fmt.Errorf("GlobalPodWatcher already started")
	}

	w.ctx, w.cancel = context.WithCancel(ctx)

	log.Info("Starting GlobalPodWatcher",
		"resyncInterval", w.resyncInterval,
		"prometheusAddr", w.prometheusAddr)

	factory := informers.NewSharedInformerFactoryWithOptions(
		w.k8sClient,
		w.resyncInterval,
		informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
			lo.LabelSelector = "app.kubernetes.io/managed-by=Helm"
		}),
	)

	w.podInformer = factory.Core().V1().Pods().Informer()

	w.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    w.handlePodAdd,
		UpdateFunc: w.handlePodUpdate,
		DeleteFunc: w.handlePodDelete,
	})

	w.collector = NewPodCollector(w.podInformer, w.filter)

	w.workerPool = NewWorkerPool(w.queue, w.k8sClient, w.collector, w.releases, w.policyEngine)

	factory.Start(w.ctx.Done())

	if !cache.WaitForCacheSync(w.ctx.Done(), w.podInformer.HasSynced) {
		w.cancel()
		return fmt.Errorf("failed to sync pod informer cache")
	}

	log.Info("Pod informer cache synced successfully")

	go w.workerPool.Start(w.ctx)

	go func() {
		if err := StartPrometheusServer(w.ctx, w.prometheusAddr, w.collector); err != nil {
			log.ErrorE(err, "Prometheus metrics server error")
		}
	}()

	w.started = true
	log.Info("GlobalPodWatcher started successfully")

	return nil
}

func (w *GlobalPodWatcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.started {
		return
	}

	w.cancel()

	w.started = false
	log.Info("GlobalPodWatcher stopped")
}

func (w *GlobalPodWatcher) RegisterRelease(releaseName string, cfg *ReleaseConfig) {
	w.releases.Register(releaseName, cfg)
	log.Info("Release registered", "release", releaseName)
}

func (w *GlobalPodWatcher) SetPolicyEngine(engine *policy.Engine) {
	w.policyEngine = engine
}

func (w *GlobalPodWatcher) MarkReleaseRolledBack(releaseName string) {
	if w.policyEngine != nil {
		w.policyEngine.MarkRolledBack(releaseName)
	}
}

func (w *GlobalPodWatcher) UnregisterRelease(releaseName string) {
	w.releases.Unregister(releaseName)
	log.Info("Release unregistered", "release", releaseName)
}

func (w *GlobalPodWatcher) UpdateReleaseConfig(releaseName string, endpoint string, method string, headers map[string]string) {
	cfg := w.releases.Get(releaseName)
	if cfg != nil && cfg.EventSender != nil {
		cfg.EventSender.UpdateConfig(endpoint, method, headers)
	}
}

func (w *GlobalPodWatcher) handlePodAdd(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		log.Info("Invalid type in AddFunc", "type", fmt.Sprintf("%T", obj))
		return
	}

	if !w.podInformer.HasSynced() {
		return
	}

	if !w.filter.ShouldMonitor(pod) {
		return
	}

	w.queue.Put(pod, PodEventAdded)

	log.Info("Pod added",
		"namespace", pod.Namespace,
		"name", pod.Name,
		"phase", pod.Status.Phase,
		"release", GetReleaseName(pod))
}

func (w *GlobalPodWatcher) handlePodUpdate(oldObj, newObj interface{}) {
	pod, ok := newObj.(*corev1.Pod)
	if !ok {
		log.Info("Invalid type in UpdateFunc", "type", fmt.Sprintf("%T", newObj))
		return
	}

	if !w.filter.ShouldMonitor(pod) {
		return
	}

	w.queue.Put(pod, PodEventModified)

	log.Debug("Pod updated",
		"namespace", pod.Namespace,
		"name", pod.Name,
		"phase", pod.Status.Phase,
		"release", GetReleaseName(pod))
}

func (w *GlobalPodWatcher) handlePodDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			log.Info("Invalid type in DeleteFunc", "type", fmt.Sprintf("%T", obj))
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			log.Info("Invalid type in DeletedFinalStateUnknown", "type", fmt.Sprintf("%T", tombstone.Obj))
			return
		}
	}

	if !w.filter.ShouldMonitor(pod) {
		return
	}

	w.queue.Put(pod, PodEventDeleted)

	log.Info("Pod deleted",
		"namespace", pod.Namespace,
		"name", pod.Name,
		"release", GetReleaseName(pod))
}

func (w *GlobalPodWatcher) GetQueueSize() int {
	return w.queue.Size()
}
