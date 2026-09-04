package queue

import (
	"errors"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func job(name string, priority int64) *ebsv1.Job {
	return &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: name, UID: types.UID("uid-" + name)}, Spec: ebsv1.JobSpec{Priority: priority}, Status: ebsv1.JobStatus{Phase: "Pending"}}
}
func TestPriorityFIFOAndDirtyWinsBackoff(t *testing.T) {
	q := New(time.Hour, time.Hour)
	q.Add(job("low", 1))
	q.Add(job("high", 2))
	first, ok := q.Pop()
	if !ok || first.Key != "p/high" {
		t.Fatalf("first=%+v", first)
	}
	updated := job("high", 3)
	q.Add(updated)
	q.AddBackoff(first, errors.New("retry"))
	again, ok := q.Pop()
	if !ok || again.Key != "p/high" || again.Retries != 0 {
		t.Fatalf("dirty event did not activate: %+v", again)
	}
	q.Done(again)
	next, _ := q.Pop()
	if next.Key != "p/low" {
		t.Fatalf("next=%+v", next)
	}
}
func TestShutdownWakesPop(t *testing.T) {
	q := New(time.Second, time.Second)
	done := make(chan bool, 1)
	go func() { _, ok := q.Pop(); done <- ok }()
	q.ShutDown()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("pop succeeded after shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("Pop did not wake")
	}
}

func TestStaleDeleteDoesNotRemoveNewUID(t *testing.T) {
	q := New(time.Second, time.Second)
	old := job("same", 1)
	q.Add(old)
	inFlight, _ := q.Pop()
	newJob := job("same", 2)
	newJob.UID = "new-uid"
	q.Add(newJob)
	q.Delete(inFlight.Key, inFlight.UID)
	q.Done(inFlight)
	if next, ok := q.Pop(); !ok || next.UID != newJob.UID {
		t.Fatalf("new object lost after stale delete: %+v", next)
	}
}

func TestMultipleWaitingPopsAreWoken(t *testing.T) {
	q := New(time.Second, time.Second)
	results := make(chan *QueuedJob, 2)
	for i := 0; i < 2; i++ {
		go func() { item, _ := q.Pop(); results <- item }()
	}
	q.Add(job("a", 1))
	q.Add(job("b", 1))
	deadline := time.After(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case item := <-results:
			if item == nil {
				t.Fatal("nil item")
			}
		case <-deadline:
			t.Fatal("waiting Pop was not woken")
		}
	}
}
