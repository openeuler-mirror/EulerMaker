package rpmrepo

import (
	"fmt"
	"log"
	"reflect"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// reconcileRelease drives the releases of one {project}/{os}/{arch} group. In-flight releases win over
// candidates that have not started yet, and at most one object advances per round.
func (r *reconciler) reconcileRelease(os, arch string) (controller.ReconcileResult, error) {
	items, err := r.controller.client.ListRpmRepos(r.ctx, r.project, metav1.ListOptions{
		FieldSelector: nonTerminalRpmRepoFieldSelector,
		LabelSelector: labels.Set{ebsv1.BuildTargetOSLabel: os, ebsv1.BuildTargetArchLabel: arch}.String(),
	})
	if err != nil {
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	sortRpmRepos(items)
	inflight := make([]ebsv1.RpmRepo, 0, len(items))
	for _, item := range items {
		if item.Status.Release != nil && item.Status.Release.Transition != nil {
			inflight = append(inflight, item)
		}
	}
	if len(inflight) > 0 {
		if len(inflight) > 1 {
			log.Printf("controller=%s key=%q reason=MultipleReleaseInFlight count=%d", Name, r.key, len(inflight))
		}
		return r.resumeRelease(inflight[0])
	}
	for i := range items {
		repo := items[i]
		if repo.DeletionTimestamp != nil {
			continue
		}
		if repo.Status.Repository == nil || repo.Status.Repository.Transition != nil || len(repo.Status.Repository.SourceJobNames) == 0 {
			continue
		}
		handled, result, err := r.tryStartRelease(repo)
		if err != nil || handled {
			return result, err
		}
	}
	return controller.ReconcileResult{}, nil
}

// releaseTarget is the validated object pair both release paths work on.
type releaseTarget struct {
	repo  *ebsv1.RpmRepo
	build *ebsv1.Build
}

// releaseLoad reports how a candidate was classified while loading it.
type releaseLoad int

const (
	releaseLoaded releaseLoad = iota
	releaseSkipped
	releaseFinished
)

// resumeRelease continues an existing checkpoint without re-reading the policy.
func (r *reconciler) resumeRelease(candidate ebsv1.RpmRepo) (controller.ReconcileResult, error) {
	target, load, result, err := r.loadReleaseTarget(candidate)
	if load != releaseLoaded {
		return result, err
	}
	// The list snapshot called this object in-flight; the reloaded object may have lost the checkpoint in the
	// meantime (external cleanup or an abandoned write). Nothing to resume then: the next poll re-evaluates it.
	if target.repo.Status.Release == nil || target.repo.Status.Release.Transition == nil {
		log.Printf("controller=%s key=%q uid=%q result=ReleaseCheckpointGone", Name, r.key, target.repo.UID)
		return controller.ReconcileResult{}, nil
	}
	transition := target.repo.Status.Release.Transition
	response, err := r.controller.artifacts.GetRelease(r.ctx, target.repo.Name)
	if err != nil {
		return r.handleReleaseError(target.repo, transition, err)
	}
	return r.handleReleaseResponse(target.repo, transition, response)
}

// tryStartRelease performs the complete recheck of one candidate before a release checkpoint is written. It
// reports whether this round handled the candidate; an unhandled candidate is skipped and the scan continues.
func (r *reconciler) tryStartRelease(candidate ebsv1.RpmRepo) (bool, controller.ReconcileResult, error) {
	target, load, result, err := r.loadReleaseTarget(candidate)
	if load != releaseLoaded {
		return load == releaseFinished, result, err
	}
	info, skip, err := r.recheckReleaseInputs(target.repo, target.build)
	if err != nil || skip {
		return err != nil, controller.ReconcileResult{}, err
	}
	return r.startRelease(target, info)
}

// loadReleaseTarget reloads the candidate and applies the checks both release paths share: object identity,
// deletion, terminal phase, the owning Build, target label agreement and the abort terminal.
func (r *reconciler) loadReleaseTarget(candidate ebsv1.RpmRepo) (*releaseTarget, releaseLoad, controller.ReconcileResult, error) {
	repo, skip, err := r.reloadReleaseTarget(candidate)
	if err != nil {
		return nil, releaseFinished, controller.ReconcileResult{}, err
	}
	if skip {
		return nil, releaseSkipped, controller.ReconcileResult{}, nil
	}
	build, err := r.controller.client.GetBuild(r.ctx, r.project, repo.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Printf("controller=%s key=%q uid=%q reason=ReleaseGroupBuildMissing", Name, r.key, repo.UID)
			return nil, releaseSkipped, controller.ReconcileResult{}, nil
		}
		return nil, releaseFinished, controller.ReconcileResult{}, classifyReadError(err)
	}
	if mismatchBuildTarget(repo, build) {
		log.Printf("controller=%s key=%q uid=%q reason=RpmRepoLabelMismatch", Name, r.key, repo.UID)
		return nil, releaseSkipped, controller.ReconcileResult{}, nil
	}
	if build.Status.Phase == ebsv1.BuildAborted {
		result, err := r.collectBuildAborted(repo)
		return nil, releaseFinished, result, err
	}
	return &releaseTarget{repo: repo, build: build}, releaseLoaded, controller.ReconcileResult{}, nil
}

// recheckReleaseInputs performs the first-release recheck: the build must be finished, no input may still be
// missing, and the object must still carry an own version without an in-flight batch.
func (r *reconciler) recheckReleaseInputs(repo *ebsv1.RpmRepo, build *ebsv1.Build) (*ebsv1.BuildInfo, bool, error) {
	info, err := r.controller.client.GetBuildInfo(r.ctx, r.project, repo.Name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, classifyReadError(err)
		}
		log.Printf("controller=%s key=%q uid=%q reason=BuildInfoNotReady", Name, r.key, repo.UID)
		return nil, true, nil
	}
	if info.Status.Phase != ebsv1.BuildInfoCompleted {
		log.Printf("controller=%s key=%q uid=%q phase=%q reason=BuildInfoNotReady", Name, r.key, repo.UID, info.Status.Phase)
		return nil, true, nil
	}
	scan, err := r.scanCandidates(repo, build)
	if err != nil {
		return nil, false, err
	}
	if len(scan.candidates) > 0 {
		r.controller.Enqueue(buildKey(r.project, repo.Name))
		return nil, true, nil
	}
	if repo.Status.Repository == nil || repo.Status.Repository.RepositoryUID == "" {
		return nil, true, nil
	}
	// A batch checkpoint written after the list read wins: this object must not start a release while its own
	// repository batch is still in flight.
	if repo.Status.Repository.Transition != nil {
		return nil, true, nil
	}
	return info, false, nil
}

// startRelease applies the publish policy and, when it says yes, writes the checkpoint and submits the release.
// Precondition: recheckReleaseInputs accepted the target, so repository.* exists, carries a version UID and has
// no in-flight batch.
func (r *reconciler) startRelease(target *releaseTarget, info *ebsv1.BuildInfo) (bool, controller.ReconcileResult, error) {
	repo, build := target.repo, target.build
	decision, err := r.controller.policy.Decide(r.ctx, PublishPolicyInput{
		Project:             r.project,
		Build:               build,
		BuildInfo:           info,
		SourceRepositoryUID: repo.Status.Repository.RepositoryUID,
		TargetOS:            build.Spec.BuildTarget.Os,
		TargetArch:          build.Spec.BuildTarget.Arch,
	})
	if err != nil {
		return true, controller.ReconcileResult{}, err
	}
	if !decision.Publish {
		log.Printf("controller=%s key=%q uid=%q reason=ReleasePolicySkipped", Name, r.key, repo.UID)
		confirmed, err := r.writeReleaseSkipped(repo)
		if err != nil || confirmed == nil {
			return true, controller.ReconcileResult{}, err
		}
		return false, controller.ReconcileResult{}, nil
	}
	transition := &ebsv1.ReleaseTransition{
		SourceRepositoryUID: repo.Status.Repository.RepositoryUID,
		ExcludeSpecs:        normalizeExcludeSpecs(decision.ExcludeSpecs),
	}
	request := repo.DeepCopy()
	request.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePending, Transition: transition, UpdatedAt: r.nowPtr()}
	confirmed, err := r.commitStatus(request, func(value *ebsv1.RpmRepo) bool {
		release := value.Status.Release
		// The frozen checkpoint is the request: phase plus the complete transition must match.
		return release != nil && release.Phase == ebsv1.RpmRepoReleasePending && reflect.DeepEqual(release.Transition, transition)
	}, "ReleaseCheckpoint")
	if err != nil || confirmed == nil {
		return true, controller.ReconcileResult{}, err
	}
	response, err := r.controller.artifacts.SubmitRelease(r.ctx, CreateReleaseRequest{
		BuildName:           confirmed.Name,
		Project:             confirmed.Namespace,
		TargetOS:            build.Spec.BuildTarget.Os,
		TargetArch:          build.Spec.BuildTarget.Arch,
		SourceRepositoryUID: transition.SourceRepositoryUID,
		ExcludeSpecs:        transition.ExcludeSpecs,
	})
	if err != nil {
		result, handleErr := r.handleReleaseError(confirmed, transition, err)
		return true, result, handleErr
	}
	result, err := r.handleReleaseResponse(confirmed, transition, response)
	return true, result, err
}

// writeReleaseSkipped records a durable, terminal decision after repository inputs have settled.
func (r *reconciler) writeReleaseSkipped(repo *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
	target := repo.DeepCopy()
	target.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped, UpdatedAt: r.nowPtr()}
	return r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		release := value.Status.Release
		return release != nil && release.Phase == ebsv1.RpmRepoReleaseSkipped && release.Transition == nil && release.ContentURL == ""
	}, "ReleaseSkipped")
}

// reloadReleaseTarget re-reads the selected object and reports whether the candidate must be skipped.
func (r *reconciler) reloadReleaseTarget(candidate ebsv1.RpmRepo) (*ebsv1.RpmRepo, bool, error) {
	repo, err := r.controller.client.GetRpmRepo(r.ctx, r.project, candidate.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		return nil, true, classifyReadError(err)
	}
	if repo.UID != candidate.UID || repo.DeletionTimestamp != nil || releaseTerminal(repo) {
		return nil, true, nil
	}
	return repo, false, nil
}

func mismatchBuildTarget(repo *ebsv1.RpmRepo, build *ebsv1.Build) bool {
	if repo.Labels[ebsv1.BuildTargetOSLabel] != build.Spec.BuildTarget.Os {
		return true
	}
	return repo.Labels[ebsv1.BuildTargetArchLabel] != build.Spec.BuildTarget.Arch
}

func (r *reconciler) handleReleaseResponse(repo *ebsv1.RpmRepo, transition *ebsv1.ReleaseTransition, response ReleaseResponse) (controller.ReconcileResult, error) {
	if code := releaseContractViolation(repo, response); code != "" {
		log.Printf("controller=%s key=%q uid=%q code=%s result=ResponseContractError", Name, r.key, repo.UID, code)
		// The release path keeps the checkpoint and lets the framework backoff retry the same decision.
		return controller.ReconcileResult{}, fmt.Errorf("artifact manager release response for %s violated the contract: %s", repo.Name, code)
	}
	switch response.State {
	case ReleaseCreating:
		if _, err := r.setReleasePhase(repo, ebsv1.RpmRepoReleaseCreating, transition); err != nil {
			return controller.ReconcileResult{}, err
		}
		return controller.ReconcileResult{RequeueAfter: pollAfter(response.PollAfterSeconds)}, nil
	case ReleasePrepared:
		updated, err := r.setReleasePhase(repo, ebsv1.RpmRepoReleasePrepared, transition)
		if err != nil {
			return controller.ReconcileResult{}, err
		}
		if updated == nil {
			return controller.ReconcileResult{}, nil
		}
		if !r.takeSameRoundAttempt() {
			log.Printf("controller=%s key=%q uid=%q result=ActivationDeferred reason=%s", Name, r.key, updated.UID, ebsv1.RpmRepoReasonReleaseFailed)
			return controller.ReconcileResult{RequeueAfter: pollAfter(response.PollAfterSeconds)}, nil
		}
		activated, err := r.controller.artifacts.ActivateRelease(r.ctx, updated.Name)
		if err != nil {
			return r.handleReleaseError(updated, transition, err)
		}
		return r.handleReleaseResponse(updated, transition, activated)
	case ReleaseReady:
		if response.ContentURL == "" {
			log.Printf("controller=%s key=%q uid=%q code=MissingContentURL reason=%s", Name, r.key, repo.UID, ebsv1.RpmRepoReasonReleaseFailed)
			return controller.ReconcileResult{}, fmt.Errorf("artifact manager reported a ready release for %s without contentURL", repo.Name)
		}
		return r.collectReleaseSuccess(repo, transition, response)
	case ReleaseDeleting:
		log.Printf("controller=%s key=%q uid=%q reason=%s", Name, r.key, repo.UID, ebsv1.RpmRepoReasonReleaseFailed)
		return r.collectReleaseFailure(repo, transition)
	case ReleaseFailed:
		if response.Failure != nil && response.Failure.Retryable {
			return r.replayRelease(repo, transition)
		}
		return r.collectReleaseFailure(repo, transition)
	default:
		log.Printf("controller=%s key=%q uid=%q state=%q code=UnsupportedState result=ResponseContractError", Name, r.key, repo.UID, response.State)
		return controller.ReconcileResult{}, fmt.Errorf("artifact manager returned an unsupported release state %q", response.State)
	}
}

// releaseContractViolation reports which part of a release response cannot be trusted.
func releaseContractViolation(repo *ebsv1.RpmRepo, response ReleaseResponse) string {
	switch {
	case response.BuildName != repo.Name:
		return "ReleaseIdentityMismatch"
	case response.Attempt < 1:
		return "InvalidAttempt"
	case response.UpdatedAt.IsZero():
		return "MissingUpdatedAt"
	}
	return ""
}

func (r *reconciler) handleReleaseError(repo *ebsv1.RpmRepo, transition *ebsv1.ReleaseTransition, err error) (controller.ReconcileResult, error) {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return controller.ReconcileResult{}, ctxErr
	}
	switch {
	case isArtifactNotFound(err):
		return r.replayRelease(repo, transition)
	case isArtifactDeleting(err):
		log.Printf("controller=%s key=%q uid=%q code=%s reason=%s", Name, r.key, repo.UID, artifactErrorCode(err), ebsv1.RpmRepoReasonReleaseFailed)
		return r.collectReleaseFailure(repo, transition)
	case isArtifactRetryable(err):
		if delay := artifactRetryAfter(err); delay > 0 {
			return controller.ReconcileResult{RequeueAfter: delay}, nil
		}
		return controller.ReconcileResult{}, err
	default:
		log.Printf("controller=%s key=%q uid=%q code=%s reason=%s", Name, r.key, repo.UID, artifactErrorCode(err), ebsv1.RpmRepoReasonReleaseFailed)
		return r.collectReleaseFailure(repo, transition)
	}
}

// replayRelease resubmits the frozen checkpoint request.
func (r *reconciler) replayRelease(repo *ebsv1.RpmRepo, transition *ebsv1.ReleaseTransition) (controller.ReconcileResult, error) {
	if !r.takeSameRoundAttempt() {
		log.Printf("controller=%s key=%q uid=%q result=ReplayDeferred reason=%s", Name, r.key, repo.UID, ebsv1.RpmRepoReasonReleaseFailed)
		return controller.ReconcileResult{RequeueAfter: r.backoffDelay(1)}, nil
	}
	build, err := r.controller.client.GetBuild(r.ctx, r.project, repo.Name)
	if err != nil {
		// The in-flight object keeps its checkpoint and stops this round: the owning Build is the only source of
		// the release target, and a missing Build is an alert rather than a framework-level failure.
		if apierrors.IsNotFound(err) {
			log.Printf("controller=%s key=%q uid=%q reason=ReleaseGroupBuildMissing", Name, r.key, repo.UID)
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	response, err := r.controller.artifacts.SubmitRelease(r.ctx, CreateReleaseRequest{
		BuildName:           repo.Name,
		Project:             repo.Namespace,
		TargetOS:            build.Spec.BuildTarget.Os,
		TargetArch:          build.Spec.BuildTarget.Arch,
		SourceRepositoryUID: transition.SourceRepositoryUID,
		ExcludeSpecs:        transition.ExcludeSpecs,
	})
	if err != nil {
		return r.handleReleaseError(repo, transition, err)
	}
	return r.handleReleaseResponse(repo, transition, response)
}

// setReleasePhase records a phase advance without touching the checkpoint. The frozen checkpoint is part of the
// write intent, so an unknown write outcome is only confirmed when the checkpoint survived unchanged.
func (r *reconciler) setReleasePhase(repo *ebsv1.RpmRepo, phase ebsv1.RpmRepoReleasePhase, transition *ebsv1.ReleaseTransition) (*ebsv1.RpmRepo, error) {
	if repo.Status.Release != nil && repo.Status.Release.Phase == phase {
		return repo, nil
	}
	target := repo.DeepCopy()
	if target.Status.Release == nil {
		target.Status.Release = &ebsv1.RpmRepoReleaseStatus{}
	}
	target.Status.Release.Phase = phase
	target.Status.Release.UpdatedAt = r.nowPtr()
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		release := value.Status.Release
		return release != nil && release.Phase == phase && reflect.DeepEqual(release.Transition, transition)
	}, "ReleasePhase")
	if err != nil {
		return nil, err
	}
	return confirmed, nil
}

func (r *reconciler) collectReleaseSuccess(repo *ebsv1.RpmRepo, transition *ebsv1.ReleaseTransition, response ReleaseResponse) (controller.ReconcileResult, error) {
	target := repo.DeepCopy()
	if target.Status.Release == nil {
		target.Status.Release = &ebsv1.RpmRepoReleaseStatus{}
	}
	target.Status.Release.Phase = ebsv1.RpmRepoReleaseReady
	target.Status.Release.SourceRepositoryUID = transition.SourceRepositoryUID
	target.Status.Release.ContentURL = response.ContentURL
	target.Status.Release.Transition = nil
	target.Status.Release.UpdatedAt = r.nowPtr()
	conditions, _ := MergeCondition(target.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionTrue, ebsv1.RpmRepoReasonReleaseActivated, "", target.Generation, r.now)
	target.Status.Conditions = conditions
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		release := value.Status.Release
		wanted := target.Status.Release
		if release == nil || release.Transition != nil {
			return false
		}
		if release.Phase != wanted.Phase || release.SourceRepositoryUID != wanted.SourceRepositoryUID || release.ContentURL != wanted.ContentURL {
			return false
		}
		return conditionMatches(value.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionTrue, ebsv1.RpmRepoReasonReleaseActivated)
	}, "ReleaseReady")
	if err != nil || confirmed == nil {
		return controller.ReconcileResult{}, err
	}
	releaseReady.Inc()
	log.Printf("controller=%s key=%q uid=%q content_url=%q reason=%s", Name, r.key, confirmed.UID, response.ContentURL, ebsv1.RpmRepoReasonReleaseActivated)
	return controller.ReconcileResult{Requeue: true}, nil
}

func (r *reconciler) collectReleaseFailure(repo *ebsv1.RpmRepo, transition *ebsv1.ReleaseTransition) (controller.ReconcileResult, error) {
	target := repo.DeepCopy()
	conditions, _ := MergeCondition(target.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonReleaseFailed, "", target.Generation, r.now)
	target.Status.Conditions = conditions
	target.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed, UpdatedAt: r.nowPtr()}
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		release := value.Status.Release
		if release == nil || release.Phase != ebsv1.RpmRepoReleaseFailed || release.Transition != nil {
			return false
		}
		return conditionMatches(value.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonReleaseFailed)
	}, "ReleaseFailure")
	if err != nil || confirmed == nil {
		return controller.ReconcileResult{}, err
	}
	releaseFailed.Inc()
	log.Printf("controller=%s key=%q uid=%q reason=%s", Name, r.key, confirmed.UID, ebsv1.RpmRepoReasonReleaseFailed)
	return controller.ReconcileResult{}, nil
}

func sortRpmRepos(items []ebsv1.RpmRepo) {
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreationTimestamp.Equal(&items[j].CreationTimestamp) {
			return items[i].CreationTimestamp.Before(&items[j].CreationTimestamp)
		}
		return items[i].Name < items[j].Name
	})
}

// collectBuildAborted registers the terminal for a build the user aborted. The repository fields, including an
// in-flight batch checkpoint, are left untouched.
func (r *reconciler) collectBuildAborted(repo *ebsv1.RpmRepo) (controller.ReconcileResult, error) {
	if releaseTerminal(repo) {
		return controller.ReconcileResult{}, nil
	}
	target := repo.DeepCopy()
	conditions, _ := MergeCondition(target.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonBuildAborted, "", target.Generation, r.now)
	target.Status.Conditions = conditions
	target.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed, UpdatedAt: r.nowPtr()}
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		if value.Status.Release == nil || value.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed || value.Status.Release.Transition != nil {
			return false
		}
		return conditionMatches(value.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonBuildAborted)
	}, "BuildAborted")
	if err != nil || confirmed == nil {
		return controller.ReconcileResult{}, err
	}
	log.Printf("controller=%s key=%q uid=%q reason=%s", Name, r.key, confirmed.UID, ebsv1.RpmRepoReasonBuildAborted)
	return controller.ReconcileResult{}, nil
}

// failureCounter selects which counter a release terminal moves.
type failureCounter int

const (
	countReleaseFailure failureCounter = iota
	countNoFailure
)

// writeReleaseFailure registers a release terminal that was decided by the repository flow. It only writes the
// release side plus the publish condition: the repository fields stay exactly as they were.
func (r *reconciler) writeReleaseFailure(repo *ebsv1.RpmRepo, reason string, counter failureCounter) (controller.ReconcileResult, error) {
	target := repo.DeepCopy()
	conditions, _ := MergeCondition(target.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, reason, "", target.Generation, r.now)
	target.Status.Conditions = conditions
	target.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed, UpdatedAt: r.nowPtr()}
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		if value.Status.Release == nil || value.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed || value.Status.Release.Transition != nil {
			return false
		}
		return conditionMatches(value.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, reason)
	}, "ReleaseFailure")
	if err != nil || confirmed == nil {
		return controller.ReconcileResult{}, err
	}
	switch counter {
	case countReleaseFailure:
		releaseFailed.Inc()
	}
	log.Printf("controller=%s key=%q uid=%q reason=%s", Name, r.key, confirmed.UID, reason)
	return controller.ReconcileResult{}, nil
}
