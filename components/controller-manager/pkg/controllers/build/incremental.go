package build

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func (r *reconciler) incrementalPackageSeeds(snapshot *ebsv1.Snapshot) ([]string, error) {
	current := snapshot.Status.PackageRepoStatuses
	seeds := make(map[string]struct{}, len(snapshot.Spec.PackageRepos))
	baseName := r.current.Status.BaseBuildRef.Name
	var baseStatuses map[string]ebsv1.PackageRepoStatus
	if baseName != "" {
		baseSnapshot, err := r.controller.client.GetSnapshot(r.ctx, r.project, baseName)
		if err != nil {
			return nil, historicalReadError("Snapshot", baseName, err)
		}
		if baseSnapshot.Status.Phase != ebsv1.SnapshotActive {
			return nil, controller.NewPermanentError(fmt.Errorf("historical Snapshot %s/%s is not Active", r.project, baseName))
		}
		if err := requirePackageStatuses(baseSnapshot); err != nil {
			return nil, err
		}
		baseStatuses = baseSnapshot.Status.PackageRepoStatuses
		baseInfo, err := r.controller.client.GetBuildInfo(r.ctx, r.project, baseName)
		if err != nil {
			return nil, historicalReadError("BuildInfo", baseName, err)
		}
		if baseInfo.Status.Phase != ebsv1.BuildInfoCompleted {
			return nil, controller.NewPermanentError(fmt.Errorf("historical BuildInfo %s/%s is not Completed", r.project, baseName))
		}
		for _, name := range baseInfo.Status.FailedPackages {
			if name == "" {
				return nil, controller.NewPermanentError(fmt.Errorf("historical BuildInfo %s/%s has an empty failed package", r.project, baseName))
			}
			seeds[name] = struct{}{}
		}
	}
	if err := requirePackageStatuses(snapshot); err != nil {
		return nil, err
	}
	for _, repo := range snapshot.Spec.PackageRepos {
		name, entry := repo.Name, current[repo.Name]
		old, found := baseStatuses[name]
		if !found || entry.CommitID != old.CommitID {
			seeds[name] = struct{}{}
		}
	}
	packages := make([]string, 0, len(seeds))
	for name := range seeds {
		if _, exists := current[name]; exists {
			packages = append(packages, name)
		}
	}
	sort.Strings(packages)
	return packages, nil
}

func requirePackageStatuses(snapshot *ebsv1.Snapshot) error {
	for _, repo := range snapshot.Spec.PackageRepos {
		if repo.Name == "" {
			return controller.NewPermanentError(fmt.Errorf("Snapshot %s/%s has an empty package name", snapshot.Namespace, snapshot.Name))
		}
		if _, exists := snapshot.Status.PackageRepoStatuses[repo.Name]; !exists {
			return controller.NewPermanentError(fmt.Errorf("active Snapshot %s/%s lacks status for package %q", snapshot.Namespace, snapshot.Name, repo.Name))
		}
	}
	return nil
}

func historicalReadError(kind, name string, err error) error {
	if apierrors.IsNotFound(err) {
		return controller.NewPermanentError(fmt.Errorf("historical %s %s is missing: %w", kind, name, err))
	}
	return classifyReadError(err)
}

// writeIncrementalPackages never replays a PUT whose outcome is unknown. It confirms the original
// package list with a GET and uses that returned object for the subsequent /status CAS.
func (r *reconciler) writeIncrementalPackages(packages []string) (*ebsv1.Build, controller.ReconcileResult, error) {
	latest, err := r.controller.client.GetBuild(r.ctx, r.project, r.current.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, controller.ReconcileResult{}, nil
		}
		return nil, controller.ReconcileResult{}, classifyReadError(err)
	}
	if latest.UID != r.current.UID || latest.Status.Phase.IsTerminal() || latest.DeletionTimestamp != nil {
		return nil, controller.ReconcileResult{}, nil
	}
	if latest.ResourceVersion != r.current.ResourceVersion || latest.Status.Phase != ebsv1.BuildPending {
		return nil, controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
	}
	request := latest.DeepCopy()
	request.Spec.Packages = packages
	updated, err := r.controller.client.UpdateBuild(r.ctx, request)
	if err == nil {
		if updated.Status.Phase.IsTerminal() || updated.DeletionTimestamp != nil {
			return nil, controller.ReconcileResult{}, nil
		}
		if updated.Status.Phase != ebsv1.BuildPending || !slices.Equal(updated.Spec.Packages, packages) {
			return nil, controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
		}
		return updated, controller.ReconcileResult{}, nil
	}
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return nil, controller.ReconcileResult{}, err
	}
	switch writeErr.Outcome {
	case clientpkg.WriteUnknown:
		confirmed, getErr := r.controller.client.GetBuild(r.ctx, r.project, r.current.Name)
		if apierrors.IsNotFound(getErr) {
			return nil, controller.ReconcileResult{}, nil
		}
		if getErr != nil {
			return nil, controller.ReconcileResult{}, classifyReadError(getErr)
		}
		if confirmed.UID != r.current.UID || confirmed.Status.Phase.IsTerminal() || confirmed.DeletionTimestamp != nil {
			return nil, controller.ReconcileResult{}, nil
		}
		if confirmed.Status.Phase == ebsv1.BuildPending && slices.Equal(confirmed.Spec.Packages, packages) {
			return confirmed, controller.ReconcileResult{}, nil
		}
		return nil, controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
	case clientpkg.WriteRejected:
		switch writeErr.StatusCode {
		case 404:
			return nil, controller.ReconcileResult{}, nil
		case 409, 412:
			return nil, controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
		case 408, 429:
			return nil, controller.ReconcileResult{}, err
		}
		if writeErr.StatusCode >= 500 && writeErr.StatusCode < 600 {
			return nil, controller.ReconcileResult{}, err
		}
		return nil, controller.ReconcileResult{}, controller.NewPermanentError(err)
	case clientpkg.WriteNotSent:
		return nil, controller.ReconcileResult{}, classifyNotSentWrite(err)
	default:
		return nil, controller.ReconcileResult{}, err
	}
}
