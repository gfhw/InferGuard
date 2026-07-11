package podwatch

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
)

type PodEventQueue struct {
	mu     sync.Mutex
	items  []*PendingPod
	index  map[string]*PendingPod
	notify chan struct{}
}

func NewPodEventQueue() *PodEventQueue {
	return &PodEventQueue{
		items:  make([]*PendingPod, 0),
		index:  make(map[string]*PendingPod),
		notify: make(chan struct{}, 1),
	}
}

func (q *PodEventQueue) Put(pod *corev1.Pod, eventType PodEventType) {
	q.mu.Lock()
	defer q.mu.Unlock()

	key := string(pod.UID)
	if key == "" {
		return
	}

	if _, ok := q.index[key]; ok {
		return
	}

	pending := &PendingPod{
		PodInfo:     ConvertToPodInfo(pod),
		Snapshot:    ConvertToPodInfo(pod),
		EventType:   eventType,
		ReleaseName: GetReleaseName(pod),
	}
	q.items = append(q.items, pending)
	q.index[key] = pending

	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *PodEventQueue) Notify() <-chan struct{} {
	return q.notify
}

func (q *PodEventQueue) DrainAll() []*PendingPod {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return nil
	}

	batch := q.items
	q.items = make([]*PendingPod, 0)
	q.index = make(map[string]*PendingPod)

	return batch
}

func (q *PodEventQueue) Size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *PodEventQueue) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = make([]*PendingPod, 0)
	q.index = make(map[string]*PendingPod)
}
