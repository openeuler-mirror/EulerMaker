package build

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// childResource describes one create-only sub resource of a Build.
type childResource[T any] struct {
	kind   string
	get    func(context.Context, string, string) (T, error)
	create func(context.Context, string, T) (T, error)
	meta   func(T) *metav1.ObjectMeta
}

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

// ensureChild implements the idempotent create protocol: read by deterministic name, create when absent,
// reuse what exists, and confirm the outcome of a create whose result is unknown.
func ensureChild[T any](ctx context.Context, key string, resource childResource[T], project, name string, build func(context.Context) (T, error)) (T, ensureOutcome) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, ensureOutcome{err: err}
	}
	current, err := resource.get(ctx, project, name)
	switch {
	case err == nil:
		return current, reuseExisting(key, resource, current, name)
	case apierrors.IsNotFound(err):
	default:
		return zero, ensureOutcome{err: classifyReadError(err)}
	}
	request, err := build(ctx)
	if err != nil {
		return zero, ensureOutcome{err: err}
	}
	created, err := resource.create(ctx, project, request)
	if err == nil {
		log.Printf("controller=%s key=%q kind=%s name=%q result=ChildCreated", Name, key, resource.kind, name)
		return created, ensureOutcome{ready: true}
	}
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return zero, ensureOutcome{err: err}
	}
	switch writeErr.Outcome {
	case clientpkg.WriteRejected:
		if writeErr.StatusCode != http.StatusConflict {
			log.Printf("controller=%s key=%q kind=%s name=%q operation=%s outcome=%s status=%d reason=CreateRejected retryable=%t",
				Name, key, resource.kind, name, writeErr.Operation, writeErr.Outcome, writeErr.StatusCode, retryableCreateRejection(writeErr.StatusCode))
			return zero, ensureOutcome{err: classifyRejectedWrite(writeErr, err)}
		}
		ensureConflicts.Inc()
		log.Printf("controller=%s key=%q kind=%s name=%q reason=EnsureConflict", Name, key, resource.kind, name)
		existing, getErr := resource.get(ctx, project, name)
		if getErr == nil {
			return existing, reuseExisting(key, resource, existing, name)
		}
		if apierrors.IsNotFound(getErr) {
			return zero, ensureOutcome{requeueAfter: conflictRequeueDelay}
		}
		return zero, ensureOutcome{err: classifyReadError(getErr)}
	case clientpkg.WriteUnknown:
		log.Printf("controller=%s key=%q kind=%s name=%q reason=EnsureUnknown", Name, key, resource.kind, name)
		existing, getErr := resource.get(ctx, project, name)
		if getErr == nil {
			return existing, reuseExisting(key, resource, existing, name)
		}
		if apierrors.IsNotFound(getErr) {
			return zero, ensureOutcome{err: err}
		}
		return zero, ensureOutcome{err: classifyReadError(getErr)}
	default:
		return zero, ensureOutcome{err: classifyNotSentWrite(err)}
	}
}

func reuseExisting[T any](key string, resource childResource[T], value T, name string) ensureOutcome {
	meta := resource.meta(value)
	if meta != nil && meta.DeletionTimestamp != nil {
		ensureTerminating.Inc()
		log.Printf("controller=%s key=%q kind=%s name=%q reason=ChildResourceTerminating", Name, key, resource.kind, name)
		return ensureOutcome{}
	}
	return ensureOutcome{ready: true}
}

func (r *reconciler) ensureSnapshot() (*ebsv1.Snapshot, ensureOutcome) {
	resource := childResource[*ebsv1.Snapshot]{
		kind:   "Snapshot",
		get:    r.controller.client.GetSnapshot,
		create: r.controller.client.CreateSnapshot,
		meta:   snapshotMeta,
	}
	return ensureChild(r.ctx, r.key, resource, r.project, r.current.Name, func(ctx context.Context) (*ebsv1.Snapshot, error) {
		project, err := r.controller.client.GetProject(ctx, r.project)
		if err != nil {
			return nil, classifyProjectError(err)
		}
		return newSnapshot(r.current, project), nil
	})
}

func (r *reconciler) ensureRpmRepo() (*ebsv1.RpmRepo, ensureOutcome) {
	resource := childResource[*ebsv1.RpmRepo]{
		kind:   "RpmRepo",
		get:    r.controller.client.GetRpmRepo,
		create: r.controller.client.CreateRpmRepo,
		meta:   rpmRepoMeta,
	}
	return ensureChild(r.ctx, r.key, resource, r.project, r.current.Name, func(ctx context.Context) (*ebsv1.RpmRepo, error) {
		base, err := r.baseRepository(ctx)
		if err != nil {
			return nil, err
		}
		return newRpmRepo(r.current, base), nil
	})
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
	resource := childResource[*ebsv1.BuildInfo]{
		kind:   "BuildInfo",
		get:    r.controller.client.GetBuildInfo,
		create: r.controller.client.CreateBuildInfo,
		meta:   buildInfoMeta,
	}
	return ensureChild(r.ctx, r.key, resource, r.project, r.current.Name, func(ctx context.Context) (*ebsv1.BuildInfo, error) {
		project, err := r.controller.client.GetProject(ctx, r.project)
		if err != nil {
			return nil, classifyProjectError(err)
		}
		return newBuildInfo(r.current, project), nil
	})
}

func snapshotMeta(value *ebsv1.Snapshot) *metav1.ObjectMeta {
	if value == nil {
		return nil
	}
	return &value.ObjectMeta
}

func rpmRepoMeta(value *ebsv1.RpmRepo) *metav1.ObjectMeta {
	if value == nil {
		return nil
	}
	return &value.ObjectMeta
}

func buildInfoMeta(value *ebsv1.BuildInfo) *metav1.ObjectMeta {
	if value == nil {
		return nil
	}
	return &value.ObjectMeta
}

func newSnapshot(build *ebsv1.Build, project *ebsv1.Project) *ebsv1.Snapshot {
	return &ebsv1.Snapshot{
		TypeMeta:   metav1.TypeMeta{APIVersion: ebsv1.SchemeGroupVersion.String(), Kind: "Snapshot"},
		ObjectMeta: metav1.ObjectMeta{Name: build.Name, Namespace: build.Namespace},
		Spec: ebsv1.SnapshotSpec{
			DefaultRef:   project.Spec.DefaultRef,
			PackageRepos: selectPackageRepos(build, project),
		},
	}
}

func newRpmRepo(build *ebsv1.Build, base repositoryBase) *ebsv1.RpmRepo {
	repo := &ebsv1.RpmRepo{
		TypeMeta:   metav1.TypeMeta{APIVersion: ebsv1.SchemeGroupVersion.String(), Kind: "RpmRepo"},
		ObjectMeta: metav1.ObjectMeta{Name: build.Name, Namespace: build.Namespace},
	}
	// The initial phase is part of the create contract: an inherited, complete baseline makes the process
	// repository readable from the start (Ready), while a round without a baseline starts as Processing.
	// The apiserver keeps this phase together with the baseline pair and drops every other status field.
	if base.repositoryUID != "" && base.contentURL != "" {
		repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{
			Phase:         ebsv1.RpmRepoReady,
			RepositoryUID: base.repositoryUID,
			ContentURL:    base.contentURL,
		}
		return repo
	}
	repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{Phase: ebsv1.RpmRepoProcessing}
	return repo
}

func newBuildInfo(build *ebsv1.Build, project *ebsv1.Project) *ebsv1.BuildInfo {
	return &ebsv1.BuildInfo{
		TypeMeta:   metav1.TypeMeta{APIVersion: ebsv1.SchemeGroupVersion.String(), Kind: "BuildInfo"},
		ObjectMeta: metav1.ObjectMeta{Name: build.Name, Namespace: build.Namespace},
		Spec:       ebsv1.BuildInfoSpec{BootstrapRepo: copyBootstrapRepos(project.Spec.BootstrapRepo)},
	}
}

// selectPackageRepos copies the Project repositories for the Snapshot input. For buildType=single only the
// deduplicated names listed in Build.spec.packages are copied, keeping the Project order.
func selectPackageRepos(build *ebsv1.Build, project *ebsv1.Project) []ebsv1.PackageRepo {
	if build.Spec.BuildType != buildTypeSingle {
		result := make([]ebsv1.PackageRepo, 0, len(project.Spec.PackageRepos))
		for _, repo := range project.Spec.PackageRepos {
			result = append(result, copyPackageRepo(repo))
		}
		return result
	}
	wanted := make(map[string]struct{}, len(build.Spec.Packages))
	for _, name := range build.Spec.Packages {
		wanted[name] = struct{}{}
	}
	result := make([]ebsv1.PackageRepo, 0, len(wanted))
	for _, repo := range project.Spec.PackageRepos {
		if _, ok := wanted[repo.Name]; ok {
			result = append(result, copyPackageRepo(repo))
		}
	}
	return result
}

func copyPackageRepo(repo ebsv1.PackageRepo) ebsv1.PackageRepo {
	result := repo
	if repo.BuildTargets != nil {
		result.BuildTargets = append([]ebsv1.BuildTarget(nil), repo.BuildTargets...)
	}
	return result
}

func copyBootstrapRepos(repos []ebsv1.BootstrapRepo) []ebsv1.BootstrapRepo {
	if repos == nil {
		return nil
	}
	return append([]ebsv1.BootstrapRepo(nil), repos...)
}

func classifyProjectError(err error) error {
	if apierrors.IsNotFound(err) {
		return controller.NewPermanentError(err)
	}
	return classifyReadError(err)
}
