package podwatch

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testPod(uid, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(uid),
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/instance": "my-release",
			},
		},
	}
}

func TestPut_DeduplicatesByUID(t *testing.T) {
	q := NewPodEventQueue()
	q.Put(testPod("uid-1", "pod-1"), PodEventAdded)
	q.Put(testPod("uid-1", "pod-1"), PodEventModified)

	if q.Size() != 1 {
		t.Fatalf("expected 1 pending item after dedup, got %d", q.Size())
	}
}

func TestPut_TypeUpgradeMerge(t *testing.T) {
	cases := []struct {
		name     string
		events   []PodEventType
		expected PodEventType
	}{
		{"added-then-modified", []PodEventType{PodEventAdded, PodEventModified}, PodEventModified},
		{"added-then-deleted", []PodEventType{PodEventAdded, PodEventDeleted}, PodEventDeleted},
		{"modified-then-deleted", []PodEventType{PodEventModified, PodEventDeleted}, PodEventDeleted},
		{"added-only", []PodEventType{PodEventAdded}, PodEventAdded},
		{"modified-then-added-is-modified", []PodEventType{PodEventModified, PodEventAdded}, PodEventModified},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewPodEventQueue()
			for _, ev := range tc.events {
				q.Put(testPod("uid-1", "pod-1"), ev)
			}
			batch := q.DrainAll()
			if len(batch) != 1 {
				t.Fatalf("expected 1 pending item, got %d", len(batch))
			}
			if batch[0].EventType != tc.expected {
				t.Fatalf("expected event type %s, got %s", tc.expected, batch[0].EventType)
			}
		})
	}
}

func TestDrainAll_EmptiesQueue(t *testing.T) {
	q := NewPodEventQueue()
	q.Put(testPod("uid-1", "pod-1"), PodEventAdded)
	q.Put(testPod("uid-2", "pod-2"), PodEventAdded)

	batch := q.DrainAll()
	if len(batch) != 2 {
		t.Fatalf("expected 2 items, got %d", len(batch))
	}
	if q.Size() != 0 {
		t.Fatalf("expected empty queue after drain, size=%d", q.Size())
	}
	if again := q.DrainAll(); again != nil {
		t.Fatalf("expected nil on empty drain, got %d items", len(again))
	}
}

func TestPut_EmptyUIDIsSkipped(t *testing.T) {
	q := NewPodEventQueue()
	q.Put(testPod("", "pod-1"), PodEventAdded)
	if q.Size() != 0 {
		t.Fatalf("expected empty UID to be skipped, size=%d", q.Size())
	}
}
