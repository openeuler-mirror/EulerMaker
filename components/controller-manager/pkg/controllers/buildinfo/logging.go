// logging.go centralizes the structured logging helpers (design 11.1/11.3):
// common fields controller/key/reason, per-(key,reason) message dedup so
// wait-type branches logged every poll round do not storm the log, and the
// once-per-process reconciled-after-start liveness marker.
package buildinfo

import (
	"fmt"
	"log"
	"strings"
	"sync"

	ebsv1 "ebs-api/ebs/v1"
)

// logDedup suppresses identical (key, reason, message) repeats (design 11.3:
// high-frequency same-key same-reason logs are aggregated per round). A
// changed message is logged again, so state transitions stay visible.
type logDedup struct {
	mu   sync.Mutex
	last map[string]string
}

var dedup = &logDedup{last: map[string]string{}}

// forgetKey drops every dedup entry of one BuildInfo key (design 5.4: the
// entries are per-object state and must not outlive the object — hooked by
// invalidateCaches and onDelete so the map never grows unbounded).
func (d *logDedup) forgetKey(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prefix := key + "\x00"
	for k := range d.last {
		if strings.HasPrefix(k, prefix) {
			delete(d.last, k)
		}
	}
}

// logf emits one structured line. Identical (key, reason, message) lines are
// logged only once until the message changes.
func (c *Controller) logf(key, reason, format string, args ...any) {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	dedup.mu.Lock()
	dedupKey := key + "\x00" + reason
	if dedup.last[dedupKey] == message {
		dedup.mu.Unlock()
		return
	}
	dedup.last[dedupKey] = message
	dedup.mu.Unlock()
	log.Printf("controller=%s key=%q reason=%s %s", Name, key, reason, message)
}

// logOnce emits one structured line without dedup: state transitions and
// terminal outcomes that must appear every time they happen (still cheap —
// they only fire on confirmed writes).
func (c *Controller) logOnce(key, reason, format string, args ...any) {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	log.Printf("controller=%s key=%q reason=%s %s", Name, key, reason, message)
}

// reconciledAfterStartOnce guards the once-per-process startup marker
// (design 11.1: emitted after the first in-process reconcile completes).
var reconciledAfterStartOnce sync.Once

// logReconciledAfterStart emits the deployment liveness marker once per
// process after the first successful reconcile round.
func (c *Controller) logReconciledAfterStart(current *ebsv1.BuildInfo) {
	reconciledAfterStartOnce.Do(func() {
		log.Printf("controller=%s event=reconciled-after-start key=%q phase=%q", Name, cacheKey(current.Namespace, current.Name), current.Status.Phase)
	})
}
