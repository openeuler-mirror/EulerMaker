package build

import (
	"context"
	"fmt"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	request "k8s.io/apiserver/pkg/endpoints/request"
)

func claimConflict(c claim) error {
	claimConflicts.Inc()
	return apierrors.NewConflict(ebsv1.Resource("builds"), c.BuildName,
		fmt.Errorf("target %s/%s/%s is occupied by build %q (claim state %s)", c.Project, c.OS, c.Arch, c.BuildName, c.State))
}

func (m *claimManager) acquire(ctx context.Context, b *ebsv1.Build) (*claimRecord, error) {
	now := m.now().UTC()
	c := claim{Project: b.Namespace, OS: b.Spec.BuildTarget.Os, Arch: b.Spec.BuildTarget.Arch, ClaimID: string(uuid.NewUUID()), BuildName: b.Name, State: reserved, CreatedAt: now, UpdatedAt: now}
	version, err := m.backend.Create(ctx, claimResource, c.id(), c.document())
	if err == nil {
		return &claimRecord{claim: c, version: version}, nil
	}
	observed, getErr := m.get(ctx, c.id())
	if getErr != nil {
		return nil, coordinationError(err)
	}
	if observed.Project != c.Project || observed.OS != c.OS || observed.Arch != c.Arch {
		return nil, coordinationError(fmt.Errorf("claim target mismatch"))
	}
	if observed.ClaimID != c.ClaimID {
		return nil, claimConflict(observed.claim)
	}
	if observed.BuildName != c.BuildName || observed.State != reserved {
		return nil, coordinationError(fmt.Errorf("reservation no longer grants admission"))
	}
	return observed, nil
}

func (m *claimManager) create(ctx context.Context, obj runtime.Object, dryRun bool, persist func() (runtime.Object, error)) (runtime.Object, error) {
	b, ok := obj.(*ebsv1.Build)
	if !ok || b == nil || b.Namespace == "" || b.Name == "" {
		return nil, apierrors.NewBadRequest("Build identity is required for admission")
	}
	ctx = request.WithNamespace(ctx, b.Namespace)
	// Preserve ordinary duplicate-name semantics before attempting target admission.
	if _, err := m.builds.Get(ctx, b.Name, &metav1.GetOptions{}); err == nil {
		return nil, apierrors.NewAlreadyExists(ebsv1.Resource("builds"), b.Name)
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	var c *claimRecord
	if limitedBuild(b) {
		if dryRun {
			existing, err := m.get(ctx, coordinationID(b.Namespace, b.Spec.BuildTarget.Os, b.Spec.BuildTarget.Arch))
			if err == nil {
				return nil, claimConflict(existing.claim)
			}
			if !es.IsStatus(err, 404) {
				return nil, coordinationError(err)
			}
		} else {
			var err error
			c, err = m.acquire(ctx, b)
			if err != nil {
				return nil, err
			}
		}
		if err := validateLatestBuild(ctx, m.list, b); err != nil {
			m.cleanup(ctx, c, "CreateNotSent")
			return nil, err
		}
		if err := validateFullBuildBaseline(ctx, m.list, b); err != nil {
			m.cleanup(ctx, c, "CreateNotSent")
			return nil, err
		}
	}
	if dryRun {
		return persist()
	}
	if err := ctx.Err(); err != nil {
		m.cleanup(ctx, c, "CreateNotSent")
		return nil, err
	}
	if c != nil {
		var err error
		c, err = m.transition(ctx, c, creating, "")
		if err != nil {
			return nil, coordinationError(err)
		} // Never confirm and then send.
	}
	result, err := persist() // Exactly one attempt; all validation already ran.
	if err != nil {
		if apierrors.IsAlreadyExists(err) || apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			m.cleanup(ctx, c, "CreateRejected")
			return nil, err
		}
		// A timeout/5xx/response decode failure may have committed. Never replay.
		observed, getErr := m.builds.Get(ctx, b.Name, &metav1.GetOptions{})
		confirmed, valid := observed.(*ebsv1.Build)
		if getErr != nil || !valid || confirmed.Namespace != b.Namespace || confirmed.Name != b.Name || confirmed.Spec.BuildTarget != b.Spec.BuildTarget || confirmed.Spec.BuildType != b.Spec.BuildType {
			return nil, err
		}
		result = observed
	}
	if c != nil {
		if _, bindErr := m.transition(ctx, c, bound, ""); bindErr != nil {
			m.logError(bindErr, "bind", c.claim)
		}
		// Handles a fast terminal status or deletion that raced with creation.
		if _, _, recoverErr := m.recoverOne(ctx, c.id(), c.ClaimID); recoverErr != nil {
			m.logError(recoverErr, "after-create", c.claim)
		}
	}
	return result, nil
}

func (m *claimManager) cleanup(ctx context.Context, c *claimRecord, reason string) {
	if c == nil {
		return
	}
	if err := m.release(ctx, c, reason); err != nil {
		m.logError(err, "release", c.claim)
	}
}
