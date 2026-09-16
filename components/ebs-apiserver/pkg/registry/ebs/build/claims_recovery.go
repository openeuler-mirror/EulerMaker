package build

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
)

func (m *claimManager) logError(err error, operation string, c claim) {
	claimRecoveryErrors.Inc()
	klog.ErrorS(err, "Build target coordination failed", "operation", operation, "project", c.Project, "os", c.OS, "arch", c.Arch, "claimID", c.ClaimID, "build", c.BuildName, "state", c.State)
}

func (m *claimManager) afterWrite(ctx context.Context, obj runtime.Object, deleted bool) {
	b, ok := obj.(*ebsv1.Build)
	if !ok || !limitedBuild(b) || (!deleted && !b.Status.Phase.IsTerminal()) {
		return
	}
	id := coordinationID(b.Namespace, b.Spec.BuildTarget.Os, b.Spec.BuildTarget.Arch)
	c, err := m.get(ctx, id)
	if es.IsStatus(err, 404) {
		return
	}
	if err != nil {
		m.logError(err, "after-write", claim{Project: b.Namespace, BuildName: b.Name})
		return
	}
	if c.BuildName != b.Name || c.Project != b.Namespace {
		return
	} // Old cleanup must not affect a new owner.
	if deleted && (c.State == creating || c.State == bound) {
		// Successful physical deletion proves this exact Build existed, even if the
		// create response and Bound transition have not completed yet.
		m.cleanup(ctx, c, "Deleted")
		return
	}
	if _, _, err := m.recoverOne(ctx, id, c.ClaimID); err != nil {
		m.logError(err, "after-write", c.claim)
	}
}

// recoverOne returns whether Creating remains unconfirmed and its age.
// Search results are never used to authorize a release: always GET current state.
func (m *claimManager) recoverOne(ctx context.Context, id string, expected ...string) (bool, time.Duration, error) {
	c, err := m.get(ctx, id)
	if es.IsStatus(err, 404) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if len(expected) > 0 && c.ClaimID != expected[0] {
		return false, 0, nil
	}
	switch c.State {
	case reserved:
		return false, 0, m.release(ctx, c, "CreateNotSent")
	case releasing:
		return false, 0, m.release(ctx, c, c.ReleaseReason)
	case creating, bound:
		b, err := m.getBuild(ctx, c.claim)
		if apierrors.IsNotFound(err) {
			if c.State == bound {
				return false, 0, m.release(ctx, c, "Deleted")
			}
			age := m.now().Sub(c.CreatedAt)
			if age < 0 {
				age = 0
			}
			klog.InfoS("Build creation remains unconfirmed; claim retained", "project", c.Project, "build", c.BuildName, "claimID", c.ClaimID, "state", c.State, "age", age)
			return true, age, nil
		}
		if err != nil {
			return false, 0, err
		}
		if c.State == creating {
			c, err = m.transition(ctx, c, bound, "")
			if err != nil {
				return false, 0, err
			}
		}
		if b.Status.Phase.IsTerminal() {
			return false, 0, m.release(ctx, c, "Terminal")
		}
		return false, 0, nil
	default:
		return false, 0, fmt.Errorf("unknown claim state %q", c.State)
	}
}

func (m *claimManager) scan(ctx context.Context) error {
	pit, err := m.backend.OpenPIT(ctx, claimResource, "5m")
	if err != nil {
		return err
	}
	defer func() { _ = m.backend.ClosePIT(ctx, pit) }()
	var after []json.RawMessage
	count := 0
	oldest := time.Duration(0)
	for {
		page, err := m.backend.SearchPIT(ctx, pit, "5m", map[string]interface{}{"match_all": map[string]interface{}{}}, 100, after)
		if err != nil {
			return err
		}
		if page.PITID != "" {
			pit = page.PITID
		}
		for _, hit := range page.Hits {
			if err := ctx.Err(); err != nil {
				return err
			}
			unknown, age, err := m.recoverOne(ctx, hit.ID)
			if err != nil && !es.IsStatus(err, 409) {
				m.logError(err, "scan", claim{ClaimID: hit.ID})
			}
			if unknown {
				count++
				if age > oldest {
					oldest = age
				}
			}
		}
		if len(page.Hits) < 100 {
			break
		}
		after = page.Hits[len(page.Hits)-1].Sort
		if len(after) == 0 {
			return fmt.Errorf("claim scan missing search_after cursor")
		}
	}
	claimUnconfirmed.Set(float64(count))
	claimOldestUnconfirmed.Set(oldest.Seconds())
	return nil
}

func (m *claimManager) run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := m.scan(ctx); err != nil && ctx.Err() == nil {
			m.logError(err, "scan", claim{})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
