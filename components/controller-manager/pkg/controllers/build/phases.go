package build

import (
	"fmt"
	"log"
	"sort"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	stageBuild   = "build"
	stagePublish = "publish"

	buildTypeSingle = "single"

	specStatusSucceeded = "Succeeded"
)

// pending fixes the base build reference once, then ensures the Snapshot (and, for non single builds, the
// RpmRepo) before advancing to Prepared.
func (r *reconciler) pending() (controller.ReconcileResult, error) {
	if r.current.Status.BaseBuildRef == nil {
		return r.recordBaseBuildRef()
	}
	snapshot, outcome := r.ensureSnapshot()
	if !outcome.ready {
		return outcome.result(), outcome.err
	}
	if snapshot.Status.Phase != ebsv1.SnapshotActive {
		return controller.ReconcileResult{}, nil
	}
	if _, outcome := r.ensureRpmRepo(); !outcome.ready {
		return outcome.result(), outcome.err
	}
	target := r.current.DeepCopy()
	target.Status.Phase = ebsv1.BuildPrepared
	intent := writeIntent{uid: target.UID, phase: target.Status.Phase, stage: target.Status.Stage}
	return r.apply(target, intent, controller.ReconcileResult{}, nil)
}

// recordBaseBuildRef resolves the last published Build of the same target. A missing history is recorded as an
// empty object, which is distinct from nil (not resolved yet). The round returns right after the write.
func (r *reconciler) recordBaseBuildRef() (controller.ReconcileResult, error) {
	reference := &ebsv1.BaseBuildRef{}
	previous, err := r.controller.client.GetLastPublishedBuild(r.ctx, r.project, r.current.Spec.BuildTarget.Os, r.current.Spec.BuildTarget.Arch)
	if err != nil {
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if previous != nil {
		reference.Name = previous.Name
	}
	target := r.current.DeepCopy()
	target.Status.BaseBuildRef = reference
	intent := writeIntent{
		uid:             target.UID,
		phase:           target.Status.Phase,
		stage:           target.Status.Stage,
		setBaseBuildRef: true,
		baseBuildRef:    reference,
	}
	return r.apply(target, intent, controller.ReconcileResult{}, nil)
}

// prepared ensures the BuildInfo and enters the build stage.
func (r *reconciler) prepared() (controller.ReconcileResult, error) {
	if _, outcome := r.ensureBuildInfo(); !outcome.ready {
		return outcome.result(), outcome.err
	}
	target := r.current.DeepCopy()
	target.Status.Phase = ebsv1.BuildProcessing
	target.Status.Stage = stageBuild
	target.Status.StartTime = r.now
	intent := writeIntent{
		uid: target.UID, phase: target.Status.Phase, stage: target.Status.Stage,
		setStartTime: true, startTime: r.now,
	}
	return r.apply(target, intent, controller.ReconcileResult{}, nil)
}

// processingBuild waits for the BuildInfo of this round and aggregates its spec results.
func (r *reconciler) processingBuild() (controller.ReconcileResult, error) {
	info, err := r.controller.client.GetBuildInfo(r.ctx, r.project, r.current.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.failChildResourceMissing(stageBuild, ConditionBuildSucceed, "BuildInfo")
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if info.Status.Phase != ebsv1.BuildInfoCompleted {
		return controller.ReconcileResult{}, nil
	}
	succeeded, failing := aggregateSpecStatus(info.Status.SpecStatus)
	if r.current.Spec.BuildType == buildTypeSingle {
		return r.finishSingleBuild(succeeded, failing)
	}
	return r.advanceNonSingleBuild(succeeded, failing)
}

// finishSingleBuild ends a single build directly: it never creates, reads or publishes a repository.
func (r *reconciler) finishSingleBuild(succeeded bool, failing string) (controller.ReconcileResult, error) {
	target := r.current.DeepCopy()
	target.Status.EndTime = r.now
	merged, intent := r.withCondition(target.Status.Conditions, ConditionBuildSucceed, conditionStatus(succeeded), buildResultReason(succeeded))
	target.Status.Conditions = merged
	if succeeded {
		// Skipping the release is the only stage advance a single build performs.
		target.Status.Stage = stagePublish
		target.Status.Phase = ebsv1.BuildSkipped
	} else {
		// A failed single build never reached the release stage, so its stage stays build.
		target.Status.Stage = stageBuild
		target.Status.Phase = ebsv1.BuildFailed
	}
	write := writeIntent{
		uid: target.UID, phase: target.Status.Phase, stage: target.Status.Stage,
		setEndTime: true, endTime: r.now,
		setConditions: true, conditions: merged, conditionIntents: []conditionIntent{intent},
	}
	if succeeded {
		return r.apply(target, write, controller.ReconcileResult{}, nil)
	}
	log.Printf("controller=%s key=%q uid=%q phase=%q stage=%q reason=%s spec=%q", Name, r.key, target.UID, target.Status.Phase, target.Status.Stage, ReasonBuildFailed, failing)
	return r.apply(target, write, controller.ReconcileResult{}, controller.NewPermanentError(
		fmt.Errorf("Build %s/%s failed for spec %q", target.Namespace, target.Name, failing)))
}

// advanceNonSingleBuild records the build result and either skips publishing or waits for the release.
// BuildSucceed reports the build result only and never gates the release.
func (r *reconciler) advanceNonSingleBuild(succeeded bool, failing string) (controller.ReconcileResult, error) {
	target := r.current.DeepCopy()
	merged, intent := r.withCondition(target.Status.Conditions, ConditionBuildSucceed, conditionStatus(succeeded), buildResultReason(succeeded))
	target.Status.Conditions = merged
	target.Status.Stage = stagePublish
	write := writeIntent{
		uid: target.UID, phase: target.Status.Phase, stage: target.Status.Stage,
		setConditions: true, conditions: merged, conditionIntents: []conditionIntent{intent},
	}
	if r.current.Spec.BuildTarget.PublishFlag {
		return r.apply(target, write, controller.ReconcileResult{}, nil)
	}
	target.Status.Phase = ebsv1.BuildSkipped
	target.Status.EndTime = r.now
	write.phase = target.Status.Phase
	write.setEndTime = true
	write.endTime = r.now
	if succeeded {
		return r.apply(target, write, controller.ReconcileResult{}, nil)
	}
	log.Printf("controller=%s key=%q uid=%q phase=%q stage=%q reason=%s spec=%q", Name, r.key, target.UID, target.Status.Phase, target.Status.Stage, ReasonBuildFailed, failing)
	return r.apply(target, write, controller.ReconcileResult{}, controller.NewPermanentError(
		fmt.Errorf("Build %s/%s failed for spec %q", target.Namespace, target.Name, failing)))
}

// processingPublish consumes the release phase of the RpmRepo created for this Build.
func (r *reconciler) processingPublish() (controller.ReconcileResult, error) {
	repo, err := r.controller.client.GetRpmRepo(r.ctx, r.project, r.current.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.failChildResourceMissing(stagePublish, ConditionPublishSucceed, "RpmRepo")
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	release := repo.Status.Release
	if release == nil || (release.Phase != ebsv1.RpmRepoReleaseReady && release.Phase != ebsv1.RpmRepoReleaseFailed) {
		return controller.ReconcileResult{}, nil
	}
	succeeded := release.Phase == ebsv1.RpmRepoReleaseReady
	target := r.current.DeepCopy()
	target.Status.Stage = stagePublish
	target.Status.EndTime = r.now
	merged, intent := r.withCondition(target.Status.Conditions, ConditionPublishSucceed, conditionStatus(succeeded), publishResultReason(succeeded))
	target.Status.Conditions = merged
	write := writeIntent{
		uid: target.UID, phase: target.Status.Phase, stage: target.Status.Stage,
		setEndTime: true, endTime: r.now,
		setConditions: true, conditions: merged, conditionIntents: []conditionIntent{intent},
	}
	if !succeeded {
		target.Status.Phase = ebsv1.BuildFailed
		write.phase = target.Status.Phase
		return r.apply(target, write, controller.ReconcileResult{}, controller.NewPermanentError(
			fmt.Errorf("release of Build %s/%s failed", target.Namespace, target.Name)))
	}
	target.Status.Phase = ebsv1.BuildSuccess
	write.phase = target.Status.Phase
	return r.apply(target, write, controller.ReconcileResult{}, nil)
}

// failChildResourceMissing records a direct dependency that disappeared while the Build was processing.
func (r *reconciler) failChildResourceMissing(stage, conditionType, kind string) (controller.ReconcileResult, error) {
	log.Printf("controller=%s key=%q uid=%q phase=%q stage=%q reason=ChildResourceMissing resource=%s name=%q", Name, r.key, r.current.UID, r.current.Status.Phase, r.current.Status.Stage, kind, r.current.Name)
	target := r.current.DeepCopy()
	target.Status.Phase = ebsv1.BuildFailed
	target.Status.Stage = stage
	target.Status.EndTime = r.now
	merged, intent := r.withCondition(target.Status.Conditions, conditionType, metav1.ConditionFalse, ReasonChildResourceMissing)
	target.Status.Conditions = merged
	write := writeIntent{
		uid: target.UID, phase: target.Status.Phase, stage: target.Status.Stage,
		setEndTime: true, endTime: r.now,
		setConditions: true, conditions: merged, conditionIntents: []conditionIntent{intent},
	}
	return r.apply(target, write, controller.ReconcileResult{}, controller.NewPermanentError(
		fmt.Errorf("%s %s/%s is missing while Build is in %s/%s", kind, r.project, r.current.Name, r.current.Status.Phase, stage)))
}

// withCondition merges one result condition and returns the intent entry describing the written fields.
func (r *reconciler) withCondition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) ([]metav1.Condition, conditionIntent) {
	merged, _ := MergeCondition(conditions, condType, status, reason, reason, r.current.Generation, r.now)
	return merged, conditionIntent{
		condType:           condType,
		status:             status,
		reason:             reason,
		message:            reason,
		observedGeneration: r.current.Generation,
	}
}

// aggregateSpecStatus reports whether every spec of a completed BuildInfo succeeded. An empty result set is a
// failure: a completed BuildInfo must describe every target spec of the build.
func aggregateSpecStatus(specStatus map[string]ebsv1.SpecStatus) (bool, string) {
	if len(specStatus) == 0 {
		return false, ""
	}
	names := make([]string, 0, len(specStatus))
	for name := range specStatus {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		status := specStatus[name]
		if status.Build.Status != specStatusSucceeded || status.Install.Status != specStatusSucceeded {
			return false, name
		}
	}
	return true, ""
}

func conditionStatus(succeeded bool) metav1.ConditionStatus {
	if succeeded {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func buildResultReason(succeeded bool) string {
	if succeeded {
		return ReasonBuildSucceeded
	}
	return ReasonBuildFailed
}

func publishResultReason(succeeded bool) string {
	if succeeded {
		return ReasonPublishSucceeded
	}
	return ReasonPublishFailed
}
