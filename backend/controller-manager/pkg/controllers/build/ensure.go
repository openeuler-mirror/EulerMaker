package build

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

type ensureOutcome struct {
	ready        bool
	requeueAfter time.Duration
	err          error
}

func (o ensureOutcome) result() controller.ReconcileResult {
	if o.requeueAfter > 0 {
		return controller.ReconcileResult{RequeueAfter: o.requeueAfter}
	}
	return controller.ReconcileResult{}
}

// childCreateOutcome reports whether the caller must confirm creation with a GET.
// The outcome is also the fallback when that confirmation returns NotFound.
func (r *reconciler) childCreateOutcome(kind string, err error) (bool, ensureOutcome) {
	if err == nil {
		log.Printf("controller=%s key=%q kind=%s name=%q result=ChildCreated", Name, r.key, kind, r.current.Name)
		return false, ensureOutcome{ready: true}
	}
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return false, ensureOutcome{err: err}
	}
	switch writeErr.Outcome {
	case clientpkg.WriteRejected:
		if writeErr.StatusCode != http.StatusConflict {
			log.Printf("controller=%s key=%q kind=%s name=%q operation=%s outcome=%s status=%d reason=CreateRejected retryable=%t",
				Name, r.key, kind, r.current.Name, writeErr.Operation, writeErr.Outcome, writeErr.StatusCode, retryableCreateRejection(writeErr.StatusCode))
			return false, ensureOutcome{err: classifyRejectedWrite(writeErr, err)}
		}
		ensureConflicts.Inc()
		log.Printf("controller=%s key=%q kind=%s name=%q reason=EnsureConflict", Name, r.key, kind, r.current.Name)
		return true, ensureOutcome{requeueAfter: conflictRequeueDelay}
	case clientpkg.WriteUnknown:
		log.Printf("controller=%s key=%q kind=%s name=%q reason=EnsureUnknown", Name, r.key, kind, r.current.Name)
		return true, ensureOutcome{err: err}
	default:
		return false, ensureOutcome{err: classifyNotSentWrite(err)}
	}
}

func confirmationReadError(err error, fallback ensureOutcome) ensureOutcome {
	if apierrors.IsNotFound(err) {
		return fallback
	}
	return ensureOutcome{err: classifyReadError(err)}
}

func (r *reconciler) reuseExisting(kind string, value metav1.Object) ensureOutcome {
	if value.GetDeletionTimestamp() != nil {
		ensureTerminating.Inc()
		log.Printf("controller=%s key=%q kind=%s name=%q reason=ChildResourceTerminating", Name, r.key, kind, r.current.Name)
		return ensureOutcome{}
	}
	return ensureOutcome{ready: true}
}

func (r *reconciler) ensureSnapshot() (*ebsv1.Snapshot, ensureOutcome) {
	if err := r.ctx.Err(); err != nil {
		return nil, ensureOutcome{err: err}
	}
	current, err := r.controller.client.GetSnapshot(r.ctx, r.project, r.current.Name)
	if err == nil {
		return current, r.reuseExisting("Snapshot", current)
	}
	if !apierrors.IsNotFound(err) {
		return nil, ensureOutcome{err: classifyReadError(err)}
	}
	project, err := r.controller.client.GetProject(r.ctx, r.project)
	if err != nil {
		return nil, ensureOutcome{err: classifyProjectError(err)}
	}
	created, err := r.controller.client.CreateSnapshot(r.ctx, r.project, newSnapshot(r.current, project))
	confirm, outcome := r.childCreateOutcome("Snapshot", err)
	if outcome.ready {
		return created, outcome
	}
	if !confirm {
		return nil, outcome
	}
	existing, err := r.controller.client.GetSnapshot(r.ctx, r.project, r.current.Name)
	if err != nil {
		return nil, confirmationReadError(err, outcome)
	}
	return existing, r.reuseExisting("Snapshot", existing)
}

func (r *reconciler) ensureRpmRepo() (*ebsv1.RpmRepo, ensureOutcome) {
	if err := r.ctx.Err(); err != nil {
		return nil, ensureOutcome{err: err}
	}
	current, err := r.controller.client.GetRpmRepo(r.ctx, r.project, r.current.Name)
	if err == nil {
		return current, r.reuseExisting("RpmRepo", current)
	}
	if !apierrors.IsNotFound(err) {
		return nil, ensureOutcome{err: classifyReadError(err)}
	}
	base, err := r.baseRepository(r.ctx)
	if err != nil {
		return nil, ensureOutcome{err: err}
	}
	created, err := r.controller.client.CreateRpmRepo(r.ctx, r.project, newRpmRepo(r.current, base))
	confirm, outcome := r.childCreateOutcome("RpmRepo", err)
	if outcome.ready {
		return created, outcome
	}
	if !confirm {
		return nil, outcome
	}
	existing, err := r.controller.client.GetRpmRepo(r.ctx, r.project, r.current.Name)
	if err != nil {
		return nil, confirmationReadError(err, outcome)
	}
	return existing, r.reuseExisting("RpmRepo", existing)
}

// repositoryBase is the process repository version a new RpmRepo inherits from the last successfully
// published Build. Both fields are kept together: a version that lacks either one is not usable.
type repositoryBase struct {
	repositoryUID string
	contentURL    string
}

// baseRepository resolves the inherited process repository version. The lookup only happens when the RpmRepo
// of this round is about to be created. Only a definitive "there is no usable baseline" degrades to an empty
// base: a missing historical RpmRepo (NotFound) or an object without a complete process repository version.
// Every other read failure is classified and returned so the round retries with backoff instead of silently
// creating a repository without the inherited baseline. An expired manager context stops the round first.
func (r *reconciler) baseRepository(ctx context.Context) (repositoryBase, error) {
	reference := r.current.Status.BaseBuildRef
	if reference == nil || reference.Name == "" {
		return repositoryBase{}, nil
	}
	previous, err := r.controller.client.GetRpmRepo(ctx, r.project, reference.Name)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return repositoryBase{}, ctxErr
		}
		if apierrors.IsNotFound(err) {
			// A missing historical RpmRepo is a definitive "no baseline": creation continues without it.
			r.logBaseRepositoryUnavailable(reference.Name, err.Error())
			return repositoryBase{}, nil
		}
		classified := classifyReadError(err)
		log.Printf("controller=%s key=%q kind=RpmRepo name=%q reason=BaseRepositoryReadFailed base_build=%q retryable=%t error=%q",
			Name, r.key, r.current.Name, reference.Name, !controller.IsPermanent(classified), err.Error())
		return repositoryBase{}, classified
	}
	repository := previous.Status.Repository
	if repository == nil || repository.RepositoryUID == "" || repository.ContentURL == "" {
		r.logBaseRepositoryUnavailable(reference.Name, "historical RpmRepo has no complete repository version")
		return repositoryBase{}, nil
	}
	return repositoryBase{repositoryUID: repository.RepositoryUID, contentURL: repository.ContentURL}, nil
}

func (r *reconciler) logBaseRepositoryUnavailable(baseBuild, detail string) {
	log.Printf("controller=%s key=%q kind=RpmRepo name=%q reason=BaseRepositoryUnavailable base_build=%q error=%q", Name, r.key, r.current.Name, baseBuild, detail)
}

func (r *reconciler) ensureBuildInfo() (*ebsv1.BuildInfo, ensureOutcome) {
	if err := r.ctx.Err(); err != nil {
		return nil, ensureOutcome{err: err}
	}
	current, err := r.controller.client.GetBuildInfo(r.ctx, r.project, r.current.Name)
	if err == nil {
		return current, r.reuseExisting("BuildInfo", current)
	}
	if !apierrors.IsNotFound(err) {
		return nil, ensureOutcome{err: classifyReadError(err)}
	}
	project, err := r.controller.client.GetProject(r.ctx, r.project)
	if err != nil {
		return nil, ensureOutcome{err: classifyProjectError(err)}
	}
	content, err := r.controller.client.GetBuildTargetContent(r.ctx)
	if err != nil {
		return nil, ensureOutcome{err: classifyReadError(err)}
	}
	if content == nil {
		return nil, ensureOutcome{err: controller.NewPermanentError(fmt.Errorf("build-target Config is unavailable"))}
	}
	image := content.Targets[project.Spec.Distro].Arches[r.current.Spec.Arch].Image
	if image == "" {
		return nil, ensureOutcome{
			err: controller.NewPermanentError(fmt.Errorf("build-target Config has no image for %s/%s", project.Spec.Distro, r.current.Spec.Arch)),
		}
	}
	created, err := r.controller.client.CreateBuildInfo(r.ctx, r.project, newBuildInfo(r.current, project, image))
	confirm, outcome := r.childCreateOutcome("BuildInfo", err)
	if outcome.ready {
		return created, outcome
	}
	if !confirm {
		return nil, outcome
	}
	existing, err := r.controller.client.GetBuildInfo(r.ctx, r.project, r.current.Name)
	if err != nil {
		return nil, confirmationReadError(err, outcome)
	}
	return existing, r.reuseExisting("BuildInfo", existing)
}

func newSnapshot(build *ebsv1.Build, project *ebsv1.Project) *ebsv1.Snapshot {
	snapshot := &ebsv1.Snapshot{
		TypeMeta:   metav1.TypeMeta{APIVersion: ebsv1.SchemeGroupVersion.String(), Kind: "Snapshot"},
		ObjectMeta: metav1.ObjectMeta{Name: build.Name, Namespace: build.Namespace},
		Spec: ebsv1.SnapshotSpec{
			DefaultRef:   project.Spec.DefaultRef,
			PackageRepos: selectPackageRepos(build, project),
		},
	}
	if ref, exists := build.Annotations[ebsv1.BuildGitRefAnnotation]; exists {
		for i := range snapshot.Spec.PackageRepos {
			snapshot.Spec.PackageRepos[i].Ref = ref
			snapshot.Spec.PackageRepos[i].CommitID = ""
		}
	}
	return snapshot
}

func newRpmRepo(build *ebsv1.Build, base repositoryBase) *ebsv1.RpmRepo {
	repo := &ebsv1.RpmRepo{
		TypeMeta: metav1.TypeMeta{APIVersion: ebsv1.SchemeGroupVersion.String(), Kind: "RpmRepo"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      build.Name,
			Namespace: build.Namespace,
			// Label the architecture for queries within the Project.
			Labels: map[string]string{
				ebsv1.BuildTargetArchLabel: build.Spec.Arch,
			},
		},
	}
	// Seed the last available immutable repository version, when present.
	if base.repositoryUID != "" && base.contentURL != "" {
		repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{
			RepositoryUID: base.repositoryUID,
			ContentURL:    base.contentURL,
		}
	}
	return repo
}

func newBuildInfo(build *ebsv1.Build, project *ebsv1.Project, image string) *ebsv1.BuildInfo {
	var bootstrapRepos []ebsv1.BootstrapRepo
	if len(project.Spec.BootstrapRepos) > 0 {
		bootstrapRepos = slices.Clone(project.Spec.BootstrapRepos)
	}
	return &ebsv1.BuildInfo{
		TypeMeta:   metav1.TypeMeta{APIVersion: ebsv1.SchemeGroupVersion.String(), Kind: "BuildInfo"},
		ObjectMeta: metav1.ObjectMeta{Name: build.Name, Namespace: build.Namespace},
		Spec: ebsv1.BuildInfoSpec{
			Image:          image,
			BuildPayload:   project.Spec.BuildPayload,
			BootstrapRepos: bootstrapRepos,
		},
	}
}

// selectPackageRepos copies the Project repositories for the Snapshot input. For buildType=single only the
// deduplicated names listed in Build.spec.packages are copied, keeping the Project order.
func selectPackageRepos(build *ebsv1.Build, project *ebsv1.Project) []ebsv1.PackageRepo {
	if build.Spec.BuildType != ebsv1.BuildTypeSingle {
		if project.Spec.PackageRepos == nil {
			return []ebsv1.PackageRepo{}
		}
		return slices.Clone(project.Spec.PackageRepos)
	}
	wanted := make(map[string]struct{}, len(build.Spec.Packages))
	for _, name := range build.Spec.Packages {
		wanted[name] = struct{}{}
	}
	result := make([]ebsv1.PackageRepo, 0, len(wanted))
	for _, repo := range project.Spec.PackageRepos {
		if _, ok := wanted[repo.Name]; ok {
			result = append(result, repo)
		}
	}
	return result
}

func classifyProjectError(err error) error {
	if apierrors.IsNotFound(err) {
		return controller.NewPermanentError(err)
	}
	return classifyReadError(err)
}
