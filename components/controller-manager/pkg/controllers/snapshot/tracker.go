package snapshot

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

type failureKey struct {
	uid  types.UID
	repo string
}

type failureTracker struct {
	mu             sync.Mutex
	counts         map[failureKey]int
	uidByObjectKey map[string]types.UID
}

func newFailureTracker() *failureTracker {
	return &failureTracker{counts: make(map[failureKey]int), uidByObjectKey: make(map[string]types.UID)}
}

func (t *failureTracker) observe(objectKey string, uid types.UID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.uidByObjectKey[objectKey]; ok && old != uid {
		t.clearUIDLocked(old)
	}
	t.uidByObjectKey[objectKey] = uid
}

func (t *failureTracker) count(uid types.UID, repo string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[failureKey{uid: uid, repo: repo}]
}

func (t *failureTracker) set(uid types.UID, repo string, count int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := failureKey{uid: uid, repo: repo}
	if count <= 0 {
		delete(t.counts, key)
	} else {
		t.counts[key] = count
	}
}

func (t *failureTracker) clearObject(objectKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if uid, ok := t.uidByObjectKey[objectKey]; ok {
		t.clearUIDLocked(uid)
		delete(t.uidByObjectKey, objectKey)
	}
}

func (t *failureTracker) clearUID(uid types.UID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearUIDLocked(uid)
	for key, value := range t.uidByObjectKey {
		if value == uid {
			delete(t.uidByObjectKey, key)
		}
	}
}

func (t *failureTracker) clearUIDLocked(uid types.UID) {
	for key := range t.counts {
		if key.uid == uid {
			delete(t.counts, key)
		}
	}
}
