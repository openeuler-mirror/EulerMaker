package snapshot

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var errPackagesRetryable = errors.New("Snapshot contains retryable package failures")

type trackerChange struct {
	repo  string
	count int
}

func (c *Controller) sync(ctx context.Context, key string) (controller.ReconcileResult, error) {
	namespace, name, ok := splitKey(key)
	if !ok {
		return controller.ReconcileResult{}, controller.NewPermanentError(fmt.Errorf("invalid Snapshot key %q", key))
	}
	snapshot, err := c.client.GetSnapshot(ctx, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.failures.clearObject(key)
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	c.failures.observe(key, snapshot.UID)
	if snapshot.DeletionTimestamp != nil || (snapshot.Status.Phase != ebsv1.SnapshotPending && snapshot.Status.Phase != ebsv1.SnapshotProcessing) {
		if snapshot.Status.Phase == ebsv1.SnapshotActive {
			c.failures.clearUID(snapshot.UID)
		}
		return controller.ReconcileResult{}, nil
	}
	if snapshot.Status.Phase == ebsv1.SnapshotPending {
		snapshot, err = c.advancePhase(ctx, snapshot)
		if err != nil {
			if apierrors.IsNotFound(err) {
				c.failures.clearObject(key)
				return controller.ReconcileResult{}, nil
			}
			if apierrors.IsConflict(err) {
				return controller.ReconcileResult{Requeue: true}, nil
			}
			if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
				return controller.ReconcileResult{}, controller.NewPermanentError(err)
			}
			return c.handleWriteError(ctx, err, nil, nil)
		}
	}
	build, err := c.client.GetBuild(ctx, namespace, name)
	if err != nil {
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	original := snapshot.DeepCopy()
	targets, missing := selectTargets(snapshot.Spec.PackageRepos, build)
	if len(snapshot.Spec.PackageRepos) == 0 {
		setCondition(&snapshot.Status.Conditions, metav1.Condition{Type: "InvalidPackageRepos", Status: metav1.ConditionTrue, Reason: "NoPackageRepos", Message: "Snapshot contains no package repositories"}, c.clock.Now())
	} else {
		removeCondition(&snapshot.Status.Conditions, "InvalidPackageRepos")
	}
	if len(missing) > 0 {
		setCondition(&snapshot.Status.Conditions, metav1.Condition{Type: "TargetPackagesNotFound", Status: metav1.ConditionTrue, Reason: "TargetPackagesNotFound", Message: sanitize("target packages not found: " + strings.Join(missing, ", "))}, c.clock.Now())
	} else {
		removeCondition(&snapshot.Status.Conditions, "TargetPackagesNotFound")
	}
	results, err := c.resolveAll(ctx, snapshot, targets)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	changes, waiting, failed := c.mergeResults(snapshot, results)
	if allTargetsComplete(snapshot, targets) {
		snapshot.Status.Phase = ebsv1.SnapshotActive
	}
	postResult := controller.ReconcileResult{}
	var postErr error
	if snapshot.Status.Phase != ebsv1.SnapshotActive {
		if waiting {
			postResult.RequeueAfter = c.config.SyncRequeueDelay
		} else if failed {
			postErr = errPackagesRetryable
		}
	}
	if reflect.DeepEqual(original.Status, snapshot.Status) {
		c.applyTrackerChanges(snapshot.UID, changes)
		if snapshot.Status.Phase == ebsv1.SnapshotActive {
			c.failures.clearUID(snapshot.UID)
		}
		return postResult, postErr
	}
	intent := snapshot.DeepCopy()
	_, err = c.client.UpdateSnapshotStatus(ctx, intent)
	if err != nil {
		return c.handleWriteError(ctx, err, intent, func() (controller.ReconcileResult, error) {
			c.applyTrackerChanges(snapshot.UID, changes)
			if snapshot.Status.Phase == ebsv1.SnapshotActive {
				c.failures.clearUID(snapshot.UID)
			}
			return postResult, postErr
		})
	}
	c.applyTrackerChanges(snapshot.UID, changes)
	if snapshot.Status.Phase == ebsv1.SnapshotActive {
		c.failures.clearUID(snapshot.UID)
	}
	return postResult, postErr
}

func (c *Controller) advancePhase(ctx context.Context, snapshot *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
	request := snapshot.DeepCopy()
	request.Status.Phase = ebsv1.SnapshotProcessing
	updated, err := c.client.UpdateSnapshotStatus(ctx, request)
	if err == nil {
		return updated, nil
	}
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteUnknown {
		return nil, err
	}
	latest, getErr := c.client.GetSnapshot(ctx, request.Namespace, request.Name)
	if getErr != nil {
		return nil, getErr
	}
	if latest.UID != request.UID {
		return nil, apierrors.NewNotFound(sourceResource(), request.Name)
	}
	if statusEquivalent(latest.Status, request.Status) {
		return latest, nil
	}
	return nil, apierrors.NewConflict(sourceResource(), request.Name, err)
}

func (c *Controller) handleWriteError(ctx context.Context, err error, intent *ebsv1.Snapshot, confirmed func() (controller.ReconcileResult, error)) (controller.ReconcileResult, error) {
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return controller.ReconcileResult{}, err
	}
	if writeErr.Outcome == clientpkg.WriteUnknown && intent != nil {
		if ctx.Err() != nil {
			return controller.ReconcileResult{}, ctx.Err()
		}
		latest, getErr := c.client.GetSnapshot(ctx, intent.Namespace, intent.Name)
		if apierrors.IsNotFound(getErr) {
			return controller.ReconcileResult{}, nil
		}
		if getErr != nil {
			return controller.ReconcileResult{}, classifyReadError(getErr)
		}
		if latest.UID != intent.UID {
			return controller.ReconcileResult{}, nil
		}
		if statusEquivalent(latest.Status, intent.Status) {
			return confirmed()
		}
		return controller.ReconcileResult{Requeue: true}, nil
	}
	if writeErr.Outcome == clientpkg.WriteRejected {
		switch writeErr.StatusCode {
		case 404:
			return controller.ReconcileResult{}, nil
		case 409, 412:
			return controller.ReconcileResult{Requeue: true}, nil
		case 400, 401, 403, 405, 410, 422:
			return controller.ReconcileResult{}, controller.NewPermanentError(err)
		}
	}
	if writeErr.Outcome == clientpkg.WriteNotSent && writeErr.StatusCode == 0 {
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
	return controller.ReconcileResult{}, err
}

func classifyReadError(err error) error {
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
		return controller.NewPermanentError(err)
	}
	return err
}

func sourceResource() schema.GroupResource {
	return schema.GroupResource{Group: "ebs", Resource: "snapshots"}
}

func splitKey(key string) (string, string, bool) {
	parts := strings.Split(key, "/")
	returnValue := len(parts) == 2 && parts[0] != "" && parts[1] != ""
	if !returnValue {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func selectTargets(repos []ebsv1.PackageRepo, build *ebsv1.Build) ([]ebsv1.PackageRepo, []string) {
	if build.Spec.BuildType != "single" {
		return append([]ebsv1.PackageRepo(nil), repos...), nil
	}
	wanted := make(map[string]struct{}, len(build.Spec.Packages))
	for _, name := range build.Spec.Packages {
		wanted[name] = struct{}{}
	}
	found := make(map[string]struct{})
	result := make([]ebsv1.PackageRepo, 0, len(wanted))
	for _, repo := range repos {
		if _, ok := wanted[repo.Name]; ok {
			result = append(result, repo)
			found[repo.Name] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for name := range wanted {
		if _, ok := found[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return result, missing
}

func allTargetsComplete(snapshot *ebsv1.Snapshot, targets []ebsv1.PackageRepo) bool {
	for _, repo := range targets {
		status, ok := snapshot.Status.PackageRepoStatuses[repo.Name]
		if !ok || (status.CommitID == "" && (status.Error == nil || status.Error.Retryable)) {
			return false
		}
	}
	return true
}

func (c *Controller) applyTrackerChanges(uid types.UID, changes []trackerChange) {
	for _, change := range changes {
		c.failures.set(uid, change.repo, change.count)
	}
}

func sanitize(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	if utf8.RuneCountInString(value) <= 200 {
		return value
	}
	runes := []rune(value)
	return string(runes[:200])
}

func setCondition(conditions *[]metav1.Condition, desired metav1.Condition, now time.Time) {
	for i := range *conditions {
		if (*conditions)[i].Type == desired.Type {
			if (*conditions)[i].Status != desired.Status {
				desired.LastTransitionTime = metav1.NewTime(now)
			} else {
				desired.LastTransitionTime = (*conditions)[i].LastTransitionTime
			}
			(*conditions)[i] = desired
			return
		}
	}
	desired.LastTransitionTime = metav1.NewTime(now)
	*conditions = append(*conditions, desired)
}

func removeCondition(conditions *[]metav1.Condition, conditionType string) {
	result := (*conditions)[:0]
	for _, condition := range *conditions {
		if condition.Type != conditionType {
			result = append(result, condition)
		}
	}
	*conditions = result
}

func statusEquivalent(a, b ebsv1.SnapshotStatus) bool {
	if a.Phase != b.Phase || !reflect.DeepEqual(normalizeStatuses(a.PackageRepoStatuses), normalizeStatuses(b.PackageRepoStatuses)) {
		return false
	}
	return reflect.DeepEqual(normalizeConditions(a.Conditions), normalizeConditions(b.Conditions))
}

func normalizeStatuses(value map[string]ebsv1.PackageRepoStatus) map[string]ebsv1.PackageRepoStatus {
	if len(value) == 0 {
		return map[string]ebsv1.PackageRepoStatus{}
	}
	return value
}

type comparableCondition struct{ Type, Status, Reason, Message string }

func normalizeConditions(value []metav1.Condition) []comparableCondition {
	result := make([]comparableCondition, 0, len(value))
	for _, item := range value {
		result = append(result, comparableCondition{item.Type, string(item.Status), item.Reason, item.Message})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Type < result[j].Type })
	return result
}

func gitErrorKind(err error) (gitserver.ErrorKind, bool) {
	var target *gitserver.Error
	if errors.As(err, &target) {
		return target.Kind, true
	}
	return gitserver.ErrorTemporary, false
}
