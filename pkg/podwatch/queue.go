package podwatch

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// PodEventQueue is a UID-deduplicating queue. A single map keyed by pod UID
// holds at most one pending entry per pod, which keeps a CrashLoopBackOff pod
// from flooding the queue with update events. The single worker then fetches
// the pod's live state from the API server when it processes the entry.
type PodEventQueue struct {
	mu     sync.Mutex
	items  map[string]*PendingPod
	notify chan struct{}
}

func NewPodEventQueue() *PodEventQueue {
	return &PodEventQueue{
		items:  make(map[string]*PendingPod),
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

	if existing, ok := q.items[key]; ok {
		// Merge instead of dropping: collapse the event type to the most severe
		// one seen so far. This keeps the dedup (one slot per pod) while keeping
		// the emitted event type accurate — e.g. ADDED then MODIFIED becomes
		// MODIFIED, and anything then DELETED becomes DELETED (terminal).
		switch eventType {
		case PodEventDeleted:
			existing.EventType = PodEventDeleted
			existing.Snapshot = ConvertToPodInfo(pod)
		case PodEventModified:
			if existing.EventType == PodEventAdded {
				existing.EventType = PodEventModified
			}
		}
		return
	}

	q.items[key] = &PendingPod{
		Namespace:   pod.Namespace,
		Name:        pod.Name,
		UID:         key,
		ReleaseName: GetReleaseName(pod),
		EventType:   eventType,
		Snapshot:    ConvertToPodInfo(pod),
	}

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

	batch := make([]*PendingPod, 0, len(q.items))
	for _, p := range q.items {
		batch = append(batch, p)
	}
	q.items = make(map[string]*PendingPod)

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
	q.items = make(map[string]*PendingPod)
}
