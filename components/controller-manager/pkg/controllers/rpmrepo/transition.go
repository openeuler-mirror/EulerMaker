// This file holds the repository (process repository) transition state machine: batch checkpointing, result
// dispatch, retry budget handling, promotion and failure collection, plus the input scan that feeds it.
package rpmrepo

import (
	"log"
	"reflect"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// advanceRepository implements the repository state machine of the design.
func (r *reconciler) advanceRepository(repo *ebsv1.RpmRepo, build *ebsv1.Build, info *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	repository := repo.Status.Repository
	if repository == nil {
		repository = &ebsv1.RpmRepoRepositoryStatus{}
	}
	if repository.Transition != nil {
		if conditionMatches(repo.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed) {
			// The abandoned batch lost its release terminal (external cleanup): only re-register the terminal.
			log.Printf("controller=%s key=%q uid=%q reason=%s", Name, r.key, repo.UID, ebsv1.RpmRepoReasonRepositoryCreationFailed)
			// The terminal was already counted when the batch was abandoned: re-registering it counts nothing.
			return r.writeReleaseFailure(repo, ebsv1.RpmRepoReasonRepositoryCreationFailed, countNoFailure)
		}
		return r.observeRepository(repo, repository.Transition, build, info)
	}
	scan, err := r.scanCandidates(repo, build)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if len(scan.candidates) == 0 {
		return r.maybeTriggerRelease(repo, build, info)
	}
	inputs := repositoryInputs(selectBatch(scan.candidates, r.controller.config.MaxJobsPerBatch))
	base := repository.RepositoryUID
	uid, err := repositoryUID(r.project, repo.Name, base, inputs)
	if err != nil {
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
	transition := &ebsv1.RepositoryTransition{Inputs: inputs, BaseRepositoryUID: base, RepositoryUID: uid}
	target := repo.DeepCopy()
	if target.Status.Repository == nil {
		target.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{}
	}
	target.Status.Repository.Transition = transition
	target.Status.Repository.UpdatedAt = r.nowPtr()
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		current := value.Status.Repository
		// The whole frozen checkpoint has to match, not just the identity: a partially written or rewritten
		// transition means this round's intent did not land.
		return current != nil && reflect.DeepEqual(current.Transition, transition)
	}, "RepositoryCheckpoint")
	if err != nil || confirmed == nil {
		return controller.ReconcileResult{}, err
	}
	return r.submitRepository(confirmed, transition, build, info)
}

// observeRepository recovers an in-flight or abandoned batch from its frozen checkpoint.
func (r *reconciler) observeRepository(repo *ebsv1.RpmRepo, transition *ebsv1.RepositoryTransition, build *ebsv1.Build, info *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	response, err := r.controller.artifacts.GetRepository(r.ctx, transition.RepositoryUID)
	if err != nil {
		return r.handleRepositoryError(repo, transition, build, info, err)
	}
	return r.handleRepositoryResponse(repo, transition, build, info, response)
}

// submitRepository submits the frozen checkpoint exactly once per round.
func (r *reconciler) submitRepository(repo *ebsv1.RpmRepo, transition *ebsv1.RepositoryTransition, build *ebsv1.Build, info *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	request := CreateRepositoryRequest{
		RepositoryUID:     transition.RepositoryUID,
		RepositoryName:    repo.Name,
		Project:           repo.Namespace,
		BuildName:         repo.Name,
		TargetOS:          build.Spec.BuildTarget.Os,
		TargetArch:        build.Spec.BuildTarget.Arch,
		BaseRepositoryUID: transition.BaseRepositoryUID,
		Manifests:         repositoryRequests(transition.Inputs),
	}
	response, err := r.controller.artifacts.SubmitRepository(r.ctx, request)
	if err != nil {
		return r.handleRepositoryError(repo, transition, build, info, err)
	}
	return r.handleRepositoryResponse(repo, transition, build, info, response)
}

func (r *reconciler) handleRepositoryResponse(repo *ebsv1.RpmRepo, transition *ebsv1.RepositoryTransition, build *ebsv1.Build, info *ebsv1.BuildInfo, response RepositoryResponse) (controller.ReconcileResult, error) {
	if code := repositoryContractViolation(transition, response); code != "" {
		attempt := response.Attempt
		if attempt < 1 {
			attempt = 1
		}
		log.Printf("controller=%s key=%q uid=%q repository_uid=%q code=%s result=ResponseContractError", Name, r.key, repo.UID, transition.RepositoryUID, code)
		return controller.ReconcileResult{RequeueAfter: r.backoffDelay(attempt)}, nil
	}
	switch response.State {
	case RepositoryReady:
		if response.ContentURL == "" {
			log.Printf("controller=%s key=%q uid=%q repository_uid=%q code=MissingContentURL attempt=%d reason=%s", Name, r.key, repo.UID, transition.RepositoryUID, response.Attempt, ebsv1.RpmRepoReasonRepositoryCreationFailed)
			return controller.ReconcileResult{RequeueAfter: r.backoffDelay(response.Attempt)}, nil
		}
		return r.collectRepositorySuccess(repo, transition, response, build, info)
	case RepositoryCreating:
		return controller.ReconcileResult{RequeueAfter: pollAfter(response.PollAfterSeconds)}, nil
	case RepositoryDeleting:
		log.Printf("controller=%s key=%q uid=%q repository_uid=%q reason=%s", Name, r.key, repo.UID, transition.RepositoryUID, ebsv1.RpmRepoReasonRepositoryCreationFailed)
		return r.collectRepositoryFailure(repo, transition)
	case RepositoryFailed:
		if response.Failure != nil && response.Failure.Retryable {
			return r.retryRepository(repo, transition, build, info, response)
		}
		if response.Failure != nil && skippableInputFailure(response.Failure.Code) && transitionHasJob(transition, response.Failure.JobName) {
			return r.skipFailedJob(repo, transition, response.Failure)
		}
		return r.collectRepositoryFailure(repo, transition)
	default:
		log.Printf("controller=%s key=%q uid=%q repository_uid=%q state=%q code=UnsupportedState result=ResponseContractError", Name, r.key, repo.UID, transition.RepositoryUID, response.State)
		return controller.ReconcileResult{RequeueAfter: r.backoffDelay(1)}, nil
	}
}

// repositoryContractViolation reports which part of a repository response cannot be trusted. A response that
// violates the contract never advances the batch.
func repositoryContractViolation(transition *ebsv1.RepositoryTransition, response RepositoryResponse) string {
	switch {
	case response.RepositoryUID != transition.RepositoryUID:
		return "RepositoryIdentityMismatch"
	case response.Attempt < 1:
		return "InvalidAttempt"
	case response.UpdatedAt.IsZero():
		return "MissingUpdatedAt"
	}
	return ""
}

// retryRepository replays the same request while the retry budget allows it, honoring the backoff window that
// is anchored on the Artifact Manager record timestamp.
func (r *reconciler) retryRepository(repo *ebsv1.RpmRepo, transition *ebsv1.RepositoryTransition, build *ebsv1.Build, info *ebsv1.BuildInfo, response RepositoryResponse) (controller.ReconcileResult, error) {
	limit := r.controller.config.MaterializeRetryLimit
	if response.Attempt >= limit+1 {
		return r.collectRepositoryFailure(repo, transition)
	}
	delay := r.backoffDelay(response.Attempt)
	if remaining := windowRemaining(response.UpdatedAt, delay, r.nowTime); remaining > 0 {
		return controller.ReconcileResult{RequeueAfter: remaining}, nil
	}
	if !r.takeSameRoundAttempt() {
		log.Printf("controller=%s key=%q uid=%q repository_uid=%q attempt=%d requeue_after=%s result=ReplayDeferred reason=%s", Name, r.key, repo.UID, transition.RepositoryUID, response.Attempt, delay, ebsv1.RpmRepoReasonRepositoryCreationFailed)
		return controller.ReconcileResult{RequeueAfter: delay}, nil
	}
	materializeRetries.Inc()
	log.Printf("controller=%s key=%q uid=%q repository_uid=%q attempt=%d requeue_after=%s reason=%s", Name, r.key, repo.UID, transition.RepositoryUID, response.Attempt, delay, ebsv1.RpmRepoReasonRepositoryCreationFailed)
	return r.submitRepository(repo, transition, build, info)
}

func (r *reconciler) handleRepositoryError(repo *ebsv1.RpmRepo, transition *ebsv1.RepositoryTransition, build *ebsv1.Build, info *ebsv1.BuildInfo, err error) (controller.ReconcileResult, error) {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return controller.ReconcileResult{}, ctxErr
	}
	switch {
	case isArtifactNotFound(err):
		// The checkpoint is durable but the record does not exist: the submission was never accepted.
		if !r.takeSameRoundAttempt() {
			log.Printf("controller=%s key=%q uid=%q repository_uid=%q result=ReplayDeferred reason=%s", Name, r.key, repo.UID, transition.RepositoryUID, ebsv1.RpmRepoReasonRepositoryCreationFailed)
			return controller.ReconcileResult{RequeueAfter: r.backoffDelay(1)}, nil
		}
		return r.submitRepository(repo, transition, build, info)
	case isArtifactDeleting(err):
		log.Printf("controller=%s key=%q uid=%q repository_uid=%q code=%s reason=%s", Name, r.key, repo.UID, transition.RepositoryUID, artifactErrorCode(err), ebsv1.RpmRepoReasonRepositoryCreationFailed)
		return r.collectRepositoryFailure(repo, transition)
	case isArtifactRetryable(err):
		delay := artifactRetryAfter(err)
		if delay <= 0 {
			delay = r.backoffDelay(1)
		}
		log.Printf("controller=%s key=%q uid=%q repository_uid=%q code=%s requeue_after=%s result=RepositoryRetry reason=%s", Name, r.key, repo.UID, transition.RepositoryUID, artifactErrorCode(err), delay, ebsv1.RpmRepoReasonRepositoryCreationFailed)
		return controller.ReconcileResult{RequeueAfter: delay}, nil
	default:
		log.Printf("controller=%s key=%q uid=%q repository_uid=%q code=%s reason=%s", Name, r.key, repo.UID, transition.RepositoryUID, artifactErrorCode(err), ebsv1.RpmRepoReasonRepositoryCreationFailed)
		return r.collectRepositoryFailure(repo, transition)
	}
}

// collectRepositorySuccess promotes the batch, clears the checkpoint and continues with the next batch or the
// release trigger.
func (r *reconciler) collectRepositorySuccess(repo *ebsv1.RpmRepo, transition *ebsv1.RepositoryTransition, response RepositoryResponse, build *ebsv1.Build, info *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	target := repo.DeepCopy()
	if target.Status.Repository == nil {
		target.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{}
	}
	target.Status.Repository.RepositoryUID = transition.RepositoryUID
	target.Status.Repository.ContentURL = response.ContentURL
	target.Status.Repository.SourceJobNames = unionSortedNames(target.Status.Repository.SourceJobNames, transition.Inputs)
	target.Status.Repository.Transition = nil
	target.Status.Repository.UpdatedAt = r.nowPtr()
	conditions, _ := MergeCondition(target.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated, "", target.Generation, r.now)
	target.Status.Conditions = conditions
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		current := value.Status.Repository
		wanted := target.Status.Repository
		if current == nil || current.Transition != nil {
			return false
		}
		if current.RepositoryUID != wanted.RepositoryUID || current.ContentURL != wanted.ContentURL ||
			!reflect.DeepEqual(current.SourceJobNames, wanted.SourceJobNames) {
			return false
		}
		return conditionMatches(value.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated)
	}, "RepositoryPromotion")
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if confirmed == nil {
		return controller.ReconcileResult{}, nil
	}
	repositoryReady.Inc()
	log.Printf("controller=%s key=%q uid=%q repository_uid=%q source_job_names=%d reason=%s", Name, r.key, confirmed.UID, transition.RepositoryUID, len(confirmed.Status.Repository.SourceJobNames), ebsv1.RpmRepoReasonRepositoryCreated)
	return r.finishRepository(confirmed, build, info)
}

// collectRepositoryFailure keeps the abandoned batch in place and registers the release terminal in the same
// write.
func (r *reconciler) collectRepositoryFailure(repo *ebsv1.RpmRepo, transition *ebsv1.RepositoryTransition) (controller.ReconcileResult, error) {
	target := repo.DeepCopy()
	if target.Status.Repository == nil {
		target.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{}
	}
	if target.Status.Repository.Transition == nil {
		target.Status.Repository.Transition = transition
	}
	target.Status.Repository.UpdatedAt = r.nowPtr()
	conditions, _ := MergeCondition(target.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed, "", target.Generation, r.now)
	conditions, _ = MergeCondition(conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed, "", target.Generation, r.now)
	target.Status.Conditions = conditions
	target.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed, UpdatedAt: r.nowPtr()}
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		current := value.Status.Repository
		if current == nil || !reflect.DeepEqual(current.Transition, target.Status.Repository.Transition) {
			return false
		}
		if value.Status.Release == nil || value.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
			return false
		}
		return conditionMatches(value.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed)
	}, "RepositoryFailure")
	if err != nil || confirmed == nil {
		return controller.ReconcileResult{}, err
	}
	repositoryFailed.Inc()
	log.Printf("controller=%s key=%q uid=%q repository_uid=%q reason=%s", Name, r.key, confirmed.UID, transition.RepositoryUID, ebsv1.RpmRepoReasonRepositoryCreationFailed)
	return controller.ReconcileResult{}, nil
}

func skippableInputFailure(code string) bool {
	switch code {
	case "ManifestNotReady", "ManifestInvalid", "ManifestContainsNoPackages", "MaterializationInputExpired", "PackageMetadataInvalid", "PackageArchitectureMismatch", "PackageConflict":
		return true
	}
	return false
}

func transitionHasJob(transition *ebsv1.RepositoryTransition, uid string) bool {
	if uid == "" {
		return false
	}
	for _, input := range transition.Inputs {
		if input.JobName == uid {
			return true
		}
	}
	return false
}

// skipFailedJob persists the offending UID before discarding this failed batch. A new cycle selects the
// remaining Jobs and derives a new repository UID; the previous ready version remains untouched.
func (r *reconciler) skipFailedJob(repo *ebsv1.RpmRepo, transition *ebsv1.RepositoryTransition, failure *FailureInfo) (controller.ReconcileResult, error) {
	target := repo.DeepCopy()
	repository := target.Status.Repository
	repository.SkippedJobNames = unionSortedStrings(repository.SkippedJobNames, failure.JobName)
	repository.Transition = nil
	repository.UpdatedAt = r.nowPtr()
	confirmed, err := r.commitStatus(target, func(value *ebsv1.RpmRepo) bool {
		return reflect.DeepEqual(value.Status, target.Status)
	}, "RepositoryInputSkipped")
	if err != nil || confirmed == nil {
		return controller.ReconcileResult{}, err
	}
	log.Printf("controller=%s key=%q uid=%q repository_uid=%q job_name=%q code=%s result=InputSkipped", Name, r.key, confirmed.UID, transition.RepositoryUID, failure.JobName, failure.Code)
	return controller.ReconcileResult{Requeue: true}, nil
}

// finishRepository decides what follows a promoted batch: another batch, a wait, or the release trigger.
func (r *reconciler) finishRepository(repo *ebsv1.RpmRepo, build *ebsv1.Build, info *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	if build == nil {
		return controller.ReconcileResult{}, nil
	}
	fresh, err := r.controller.client.GetRpmRepo(r.ctx, r.project, repo.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	scan, err := r.scanCandidates(fresh, build)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if len(scan.candidates) > 0 {
		return controller.ReconcileResult{Requeue: true}, nil
	}
	return r.maybeTriggerRelease(fresh, build, info)
}

// maybeTriggerRelease enqueues the release key, or registers "no publishable artifacts" once the build really
// produced nothing for this repository.
func (r *reconciler) maybeTriggerRelease(repo *ebsv1.RpmRepo, build *ebsv1.Build, info *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	if info == nil || info.Status.Phase != ebsv1.BuildInfoCompleted {
		log.Printf("controller=%s key=%q uid=%q reason=BuildInfoNotReady", Name, r.key, repo.UID)
		return controller.ReconcileResult{}, nil
	}
	if !build.Spec.BuildTarget.PublishFlag {
		_, err := r.writeReleaseSkipped(repo)
		return controller.ReconcileResult{}, err
	}
	if repo.Status.Repository == nil || len(repo.Status.Repository.SourceJobNames) == 0 {
		return r.writeReleaseFailure(repo, ebsv1.RpmRepoReasonNoPublishableArtifacts, countReleaseFailure)
	}
	r.controller.Enqueue(releaseKey(r.project, build.Spec.BuildTarget.Os, build.Spec.BuildTarget.Arch))
	return controller.ReconcileResult{}, nil
}

// candidateScan is the result of reading every input of one repository round.
type candidateScan struct {
	candidates []candidate
}

// scanCandidates lists Succeeded Jobs without reading manifests; Artifact Manager validates batch inputs.
func (r *reconciler) scanCandidates(repo *ebsv1.RpmRepo, build *ebsv1.Build) (candidateScan, error) {
	jobs, err := r.controller.client.ListJobs(r.ctx, r.project, metav1.ListOptions{
		LabelSelector: labels.Set{ebsv1.JobBuildNameLabel: repo.Name}.String(),
	})
	if err != nil {
		return candidateScan{}, classifyReadError(err)
	}
	consumed := make(map[string]struct{})
	if repo.Status.Repository != nil {
		for _, uid := range repo.Status.Repository.SourceJobNames {
			consumed[uid] = struct{}{}
		}
		for _, uid := range repo.Status.Repository.SkippedJobNames {
			consumed[uid] = struct{}{}
		}
		if repo.Status.Repository.Transition != nil {
			for _, input := range repo.Status.Repository.Transition.Inputs {
				consumed[input.JobName] = struct{}{}
			}
		}
	}
	scan := candidateScan{}
	ordered := append([]ebsv1.Job(nil), jobs...)
	sortJobs(ordered)
	selectedSpecs := make(map[string]struct{})
	for i := range ordered {
		job := &ordered[i]
		if job.Status.Phase != ebsv1.JobSucceeded {
			continue
		}
		if _, exists := consumed[job.Name]; exists {
			continue
		}
		specName := job.Labels[ebsv1.JobSpecNameLabel]
		if specName == "" || job.Labels[ebsv1.BuildTargetOSLabel] != build.Spec.BuildTarget.Os || job.Labels[ebsv1.BuildTargetArchLabel] != build.Spec.BuildTarget.Arch {
			log.Printf("controller=%s key=%q uid=%q job_name=%q reason=InputLabelMismatch", Name, r.key, repo.UID, job.UID)
			continue
		}
		scan.candidates = append(scan.candidates, candidate{
			uid:       string(job.UID),
			name:      job.Name,
			specName:  specName,
			createdAt: job.CreationTimestamp.UnixNano(),
		})
		if _, exists := selectedSpecs[specName]; exists {
			continue
		}
		selectedSpecs[specName] = struct{}{}
		if len(selectedSpecs) >= r.controller.config.MaxJobsPerBatch {
			return scan, nil
		}
	}
	return scan, nil
}

func sortJobs(jobs []ebsv1.Job) {
	sort.SliceStable(jobs, func(i, j int) bool {
		if !jobs[i].CreationTimestamp.Equal(&jobs[j].CreationTimestamp) {
			return jobs[i].CreationTimestamp.Before(&jobs[j].CreationTimestamp)
		}
		if jobs[i].Name != jobs[j].Name {
			return jobs[i].Name < jobs[j].Name
		}
		return string(jobs[i].UID) < string(jobs[j].UID)
	})
}
