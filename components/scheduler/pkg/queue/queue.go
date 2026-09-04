package queue

import (
	"container/heap"
	"math/rand"
	"sync"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	"k8s.io/apimachinery/pkg/types"
)

type QueuedJob struct {
	Key         string
	UID         types.UID
	Priority    int64
	Sequence    uint64
	Retries     int
	ReadyAt     time.Time
	Schedulable bool
	index       int
}

type SchedulingQueue interface {
	Add(*ebsv1.Job)
	AddBackoff(*QueuedJob, error)
	ActivateAll()
	Delete(string, types.UID)
	Pop() (*QueuedJob, bool)
	Done(*QueuedJob)
	ShutDown()
}

type jobHeap struct {
	items  []*QueuedJob
	active bool
}

func (h jobHeap) Len() int { return len(h.items) }
func (h jobHeap) Less(i, j int) bool {
	if h.active {
		if h.items[i].Priority != h.items[j].Priority {
			return h.items[i].Priority > h.items[j].Priority
		}
		return h.items[i].Sequence < h.items[j].Sequence
	}
	if !h.items[i].ReadyAt.Equal(h.items[j].ReadyAt) {
		return h.items[i].ReadyAt.Before(h.items[j].ReadyAt)
	}
	return h.items[i].Sequence < h.items[j].Sequence
}
func (h jobHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].index = i
	h.items[j].index = j
}
func (h *jobHeap) Push(x any) {
	q := x.(*QueuedJob)
	q.index = len(h.items)
	h.items = append(h.items, q)
}
func (h *jobHeap) Pop() any {
	old := h.items
	q := old[len(old)-1]
	q.index = -1
	h.items = old[:len(old)-1]
	return q
}

type Queue struct {
	mu                        sync.Mutex
	active, backoff           jobHeap
	activeByKey, backoffByKey map[string]*QueuedJob
	inFlight, dirty           map[string]*QueuedJob
	activated                 map[string]types.UID
	nextSequence              uint64
	initial, maximum          time.Duration
	now                       func() time.Time
	rand                      *rand.Rand
	notify                    chan struct{}
	shutdown                  chan struct{}
	closed                    bool
}

func New(initial, maximum time.Duration) *Queue {
	return &Queue{active: jobHeap{active: true}, backoff: jobHeap{}, activeByKey: map[string]*QueuedJob{}, backoffByKey: map[string]*QueuedJob{}, inFlight: map[string]*QueuedJob{}, dirty: map[string]*QueuedJob{}, activated: map[string]types.UID{}, initial: initial, maximum: maximum, now: time.Now, rand: rand.New(rand.NewSource(time.Now().UnixNano())), notify: make(chan struct{}, 1), shutdown: make(chan struct{})}
}

func fromJob(job *ebsv1.Job) *QueuedJob {
	return &QueuedJob{Key: job.Namespace + "/" + job.Name, UID: job.UID, Priority: job.Spec.Priority, Schedulable: job.Status.Phase == "Pending" && job.Status.Runner == "", index: -1}
}
func clone(q *QueuedJob) *QueuedJob {
	if q == nil {
		return nil
	}
	c := *q
	c.index = -1
	return &c
}
func (q *Queue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}
func (q *Queue) sequence(item *QueuedJob) { q.nextSequence++; item.Sequence = q.nextSequence }
func (q *Queue) pushActive(item *QueuedJob) {
	q.sequence(item)
	item.ReadyAt = time.Time{}
	heap.Push(&q.active, item)
	q.activeByKey[item.Key] = item
	q.signal()
}
func (q *Queue) removeActive(item *QueuedJob) {
	heap.Remove(&q.active, item.index)
	delete(q.activeByKey, item.Key)
}
func (q *Queue) removeBackoff(item *QueuedJob) {
	heap.Remove(&q.backoff, item.index)
	delete(q.backoffByKey, item.Key)
}

func (q *Queue) Add(job *ebsv1.Job) {
	if job == nil {
		return
	}
	incoming := fromJob(job)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if flight := q.inFlight[incoming.Key]; flight != nil {
		q.dirty[incoming.Key] = incoming
		if flight.UID != incoming.UID {
			delete(q.activated, incoming.Key)
		}
		return
	}
	if current := q.activeByKey[incoming.Key]; current != nil {
		if current.UID != incoming.UID {
			q.removeActive(current)
			delete(q.activated, incoming.Key)
			if incoming.Schedulable {
				q.pushActive(incoming)
			}
			return
		}
		if !incoming.Schedulable {
			q.removeActive(current)
			return
		}
		current.Priority = incoming.Priority
		current.Schedulable = true
		heap.Fix(&q.active, current.index)
		q.signal()
		return
	}
	if current := q.backoffByKey[incoming.Key]; current != nil {
		q.removeBackoff(current)
		if incoming.Schedulable {
			incoming.Retries = 0
			q.pushActive(incoming)
		}
		return
	}
	if incoming.Schedulable {
		q.pushActive(incoming)
	}
}

func (q *Queue) Delete(key string, uid types.UID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if x := q.activeByKey[key]; x != nil && x.UID == uid {
		q.removeActive(x)
	}
	if x := q.backoffByKey[key]; x != nil && x.UID == uid {
		q.removeBackoff(x)
	}
	if q.activated[key] == uid {
		delete(q.activated, key)
	}
	if item := q.dirty[key]; item != nil && item.UID == uid {
		delete(q.dirty, key)
	}
	if x := q.inFlight[key]; x != nil && x.UID == uid {
		if latest := q.dirty[key]; latest == nil || latest.UID == uid {
			q.dirty[key] = &QueuedJob{Key: key, UID: uid, Schedulable: false, index: -1}
		}
	}
}

func (q *Queue) promote(now time.Time) {
	for q.backoff.Len() > 0 && !q.backoff.items[0].ReadyAt.After(now) {
		item := heap.Pop(&q.backoff).(*QueuedJob)
		delete(q.backoffByKey, item.Key)
		q.pushActive(item)
	}
}
func (q *Queue) Pop() (*QueuedJob, bool) {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return nil, false
		}
		q.promote(q.now())
		if q.active.Len() > 0 {
			item := heap.Pop(&q.active).(*QueuedJob)
			delete(q.activeByKey, item.Key)
			q.inFlight[item.Key] = item
			if q.active.Len() > 0 {
				q.signal()
			}
			q.mu.Unlock()
			return clone(item), true
		}
		var timer <-chan time.Time
		var t *time.Timer
		if q.backoff.Len() > 0 {
			d := time.Until(q.backoff.items[0].ReadyAt)
			if d < 0 {
				d = 0
			}
			t = time.NewTimer(d)
			timer = t.C
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
			if t != nil && !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		case <-timer:
		case <-q.shutdown:
			if t != nil {
				t.Stop()
			}
			return nil, false
		}
	}
}

func (q *Queue) Done(item *QueuedJob) {
	if item == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	flight := q.inFlight[item.Key]
	if flight == nil || flight.UID != item.UID {
		return
	}
	delete(q.inFlight, item.Key)
	dirty := q.dirty[item.Key]
	delete(q.dirty, item.Key)
	delete(q.activated, item.Key)
	if dirty != nil && dirty.Schedulable {
		dirty.Retries = 0
		q.pushActive(dirty)
	}
}

func (q *Queue) AddBackoff(item *QueuedJob, _ error) {
	if item == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	flight := q.inFlight[item.Key]
	if flight == nil || flight.UID != item.UID {
		return
	}
	delete(q.inFlight, item.Key)
	dirty := q.dirty[item.Key]
	delete(q.dirty, item.Key)
	activated := q.activated[item.Key] == flight.UID
	delete(q.activated, item.Key)
	if dirty != nil {
		if dirty.Schedulable {
			dirty.Retries = 0
			q.pushActive(dirty)
		}
		return
	}
	if activated {
		q.pushActive(flight)
		return
	}
	flight.Retries++
	delay := q.initial
	for i := 1; i < flight.Retries && delay < q.maximum; i++ {
		delay *= 2
		if delay > q.maximum {
			delay = q.maximum
		}
	}
	if delay > 0 {
		delay = time.Duration(float64(delay) * (0.9 + q.rand.Float64()*0.2))
		if delay > q.maximum {
			delay = q.maximum
		}
	}
	flight.ReadyAt = q.now().Add(delay)
	q.sequence(flight)
	heap.Push(&q.backoff, flight)
	q.backoffByKey[flight.Key] = flight
	q.signal()
}

func (q *Queue) ActivateAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.backoff.Len() > 0 {
		item := heap.Pop(&q.backoff).(*QueuedJob)
		delete(q.backoffByKey, item.Key)
		q.pushActive(item)
	}
	for key, item := range q.inFlight {
		q.activated[key] = item.UID
	}
}
func (q *Queue) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.shutdown)
}
func (q *Queue) Depths() (int, int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.active.Len(), q.backoff.Len(), len(q.inFlight)
}
