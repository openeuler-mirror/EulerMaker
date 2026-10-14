package buildinfo

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"k8s.io/klog/v2"

	ebsv1 "ebs-api/ebs/v1"
)

// logDedup suppresses identical (key, reason, message) repeats. A changed message is logged again, so state transitions
// stay visible.
type logDedup struct {
	mu   sync.Mutex
	last map[string]string
}

var dedup = &logDedup{last: map[string]string{}}

// forgetKey drops every dedup entry of one BuildInfo key when the object is invalidated or leaves the polling source.
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

// logf emits one structured line. Identical (key, reason, message) lines are logged only once until the message
// changes.
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

// logOnce emits one structured line without dedup: state transitions and terminal outcomes that must appear every time
// they happen (still cheap — they only fire on confirmed writes).
func (c *Controller) logOnce(key, reason, format string, args ...any) {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	log.Printf("controller=%s key=%q reason=%s %s", Name, key, reason, message)
}

// logBreakPoints keeps normal graph logs bounded; full names require debug verbosity.
func (c *Controller) logBreakPoints(key, reason, summary string, names []string) {
	if klog.V(4).Enabled() {
		c.logOnce(key, reason, "%s, break points: %s", summary, strings.Join(names, ","))
		return
	}
	c.logOnce(key, reason, "%s", summary)
}

// reconciledAfterStartOnce guards the once-per-process startup marker after the first in-process reconcile completes.
var reconciledAfterStartOnce sync.Once

// logReconciledAfterStart emits the deployment liveness marker once per process after the first successful reconcile
// round.
func (c *Controller) logReconciledAfterStart(current *ebsv1.BuildInfo) {
	reconciledAfterStartOnce.Do(func() {
		log.Printf(
			"controller=%s event=reconciled-after-start key=%q phase=%q",
			Name,
			cacheKey(current.Namespace, current.Name),
			current.Status.Phase,
		)
	})
}
