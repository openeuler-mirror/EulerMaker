package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
)

const (
	claimResource = "buildclaim"
	reserved      = "Reserved"
	creating      = "Creating"
	bound         = "Bound"
	releasing     = "Releasing"
)

type coordinationBackend interface {
	Create(context.Context, string, string, es.Document) (es.Version, error)
	Get(context.Context, string, string) (*es.Hit, error)
	Update(context.Context, string, string, es.Document, int64, int64) (es.Version, error)
	Delete(context.Context, string, string, int64, int64) error
	OpenPIT(context.Context, string, string) (string, error)
	SearchPIT(context.Context, string, string, map[string]interface{}, int64, []json.RawMessage) (*es.SearchResult, error)
	ClosePIT(context.Context, string) error
}

type claim struct {
	Project       string    `json:"project"`
	Arch          string    `json:"arch"`
	ClaimID       string    `json:"claimID"`
	BuildName     string    `json:"buildName"`
	State         string    `json:"state"`
	ReleaseReason string    `json:"releaseReason,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type claimRecord struct {
	claim
	version es.Version
}

type buildQueries interface {
	rest.Lister
	Exists(context.Context, *internalversion.ListOptions) (bool, error)
}

// ClaimManager coordinates Build target ownership and recovers interrupted claims.
type ClaimManager struct {
	backend coordinationBackend
	builds  rest.Getter
	list    buildQueries
	now     func() time.Time
}

// NewClaimManager attaches claim acquisition and release hooks to the Build store.
func NewClaimManager(store *esstore.Store, backend coordinationBackend) *ClaimManager {
	manager := newClaimManager(backend, store, store)
	store.SetCreateTransaction(manager.create)
	store.SetAfterWrite(manager.afterWrite)
	return manager
}

func newClaimManager(backend coordinationBackend, builds rest.Getter, list buildQueries) *ClaimManager {
	registerClaimMetrics()
	return &ClaimManager{backend: backend, builds: builds, list: list, now: time.Now}
}

func coordinationID(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c claim) id() string { return coordinationID(c.Project, c.Arch) }

func coordinationDocument(id, project, kind string, created time.Time, value interface{}) es.Document {
	data, _ := json.Marshal(value) // Only the concrete string/time records above are used.
	return es.Document{APIVersion: "internal/v1", Kind: kind, DocumentID: id,
		Metadata: es.Metadata{Name: id, Namespace: project, CreationTimestamp: created.Format(time.RFC3339Nano)}, Data: data}
}

func (c claim) document() es.Document {
	return coordinationDocument(c.id(), c.Project, "BuildTargetClaim", c.CreatedAt, c)
}

func (m *ClaimManager) get(ctx context.Context, id string) (*claimRecord, error) {
	hit, err := m.backend.Get(ctx, claimResource, id)
	if err != nil {
		return nil, err
	}
	var c claim
	if err := json.Unmarshal(hit.Document.Data, &c); err != nil {
		return nil, err
	}
	if c.id() != id || c.ClaimID == "" || c.Project == "" || c.BuildName == "" {
		return nil, fmt.Errorf("invalid claim identity for %s", id)
	}
	return &claimRecord{claim: c, version: es.Version{SeqNo: hit.SeqNo, PrimaryTerm: hit.PrimaryTerm}}, nil
}

// transition never retries: in particular, an unknown Creating CAS cannot grant
// permission to send a Build create, even if a subsequent GET finds Creating.
func (m *ClaimManager) transition(ctx context.Context, c *claimRecord, state, reason string) (*claimRecord, error) {
	next := c.claim
	next.State = state
	next.ReleaseReason = reason
	next.UpdatedAt = m.now().UTC()
	version, err := m.backend.Update(ctx, claimResource, c.id(), next.document(), c.version.SeqNo, c.version.PrimaryTerm)
	if err != nil {
		return nil, err
	}
	return &claimRecord{claim: next, version: version}, nil
}

func (m *ClaimManager) release(ctx context.Context, c *claimRecord, reason string) error {
	if c.State != releasing {
		next, err := m.transition(ctx, c, releasing, reason)
		if err != nil {
			return err
		}
		c = next
	}
	err := m.backend.Delete(ctx, claimResource, c.id(), c.version.SeqNo, c.version.PrimaryTerm)
	if es.IsStatus(err, 404) {
		return nil
	}
	return err
}

func (m *ClaimManager) getBuild(ctx context.Context, c claim) (*ebsv1.Build, error) {
	obj, err := m.builds.Get(request.WithNamespace(ctx, c.Project), c.BuildName, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	b, ok := obj.(*ebsv1.Build)
	if !ok || b == nil {
		return nil, fmt.Errorf("expected Build, got %T", obj)
	}
	if b.Namespace != c.Project || b.Name != c.BuildName || b.Spec.Arch != c.Arch || !limitedBuild(b) {
		return nil, fmt.Errorf("Build identity/target mismatch for claim %s", c.ClaimID)
	}
	return b, nil
}

func coordinationError(err error) error {
	if es.IsStatus(err, 409) {
		return apierrors.NewConflict(ebsv1.Resource("builds"), "", fmt.Errorf("build admission raced with recovery; retry with a new name"))
	}
	return apierrors.NewServiceUnavailable("build coordination unavailable: " + err.Error())
}

func claimConflict(c claim) error {
	claimConflicts.Inc()
	return apierrors.NewConflict(ebsv1.Resource("builds"), c.BuildName,
		fmt.Errorf("target %s/%s is occupied by build %q (claim state %s)", c.Project, c.Arch, c.BuildName, c.State))
}

func (m *ClaimManager) acquire(ctx context.Context, b *ebsv1.Build) (*claimRecord, error) {
	now := m.now().UTC()
	c := claim{Project: b.Namespace, Arch: b.Spec.Arch, ClaimID: string(uuid.NewUUID()), BuildName: b.Name, State: reserved, CreatedAt: now, UpdatedAt: now}
	version, err := m.backend.Create(ctx, claimResource, c.id(), c.document())
	if err == nil {
		return &claimRecord{claim: c, version: version}, nil
	}
	observed, getErr := m.get(ctx, c.id())
	if getErr != nil {
		return nil, coordinationError(err)
	}
	if observed.Project != c.Project || observed.Arch != c.Arch {
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

func (m *ClaimManager) create(ctx context.Context, obj runtime.Object, dryRun bool, persist func() (runtime.Object, error)) (runtime.Object, error) {
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
			existing, err := m.get(ctx, coordinationID(b.Namespace, b.Spec.Arch))
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
		if getErr != nil || !valid || confirmed.Namespace != b.Namespace || confirmed.Name != b.Name ||
			confirmed.Spec.Arch != b.Spec.Arch || confirmed.Spec.BuildType != b.Spec.BuildType ||
			!apiequality.Semantic.DeepEqual(confirmed.Spec.Publish, b.Spec.Publish) {
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

func (m *ClaimManager) cleanup(ctx context.Context, c *claimRecord, reason string) {
	if c == nil {
		return
	}
	if err := m.release(ctx, c, reason); err != nil {
		m.logError(err, "release", c.claim)
	}
}

var (
	claimMetricsOnce       sync.Once
	claimConflicts         = metrics.NewCounter(&metrics.CounterOpts{Name: "ebs_build_claim_conflicts_total", Help: "Build target admission conflicts."})
	claimRecoveryErrors    = metrics.NewCounter(&metrics.CounterOpts{Name: "ebs_build_claim_recovery_errors_total", Help: "Build claim recovery and cleanup errors."})
	claimUnconfirmed       = metrics.NewGauge(&metrics.GaugeOpts{Name: "ebs_build_claim_unconfirmed", Help: "Unconfirmed Creating claims observed by the last complete scan."})
	claimOldestUnconfirmed = metrics.NewGauge(&metrics.GaugeOpts{Name: "ebs_build_claim_oldest_unconfirmed_seconds", Help: "Age of the oldest unconfirmed Creating claim observed by the last complete scan."})
)

func registerClaimMetrics() {
	claimMetricsOnce.Do(func() {
		legacyregistry.MustRegister(claimConflicts, claimRecoveryErrors, claimUnconfirmed, claimOldestUnconfirmed)
	})
}

func (m *ClaimManager) logError(err error, operation string, c claim) {
	claimRecoveryErrors.Inc()
	klog.ErrorS(err, "Build target coordination failed", "operation", operation, "project", c.Project, "arch", c.Arch, "claimID", c.ClaimID, "build", c.BuildName, "state", c.State)
}

func (m *ClaimManager) afterWrite(ctx context.Context, obj runtime.Object, deleted bool) {
	b, ok := obj.(*ebsv1.Build)
	if !ok || !limitedBuild(b) || (!deleted && !b.Status.Phase.IsTerminal()) {
		return
	}
	id := coordinationID(b.Namespace, b.Spec.Arch)
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
func (m *ClaimManager) recoverOne(ctx context.Context, id string, expected ...string) (bool, time.Duration, error) {
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

func (m *ClaimManager) scan(ctx context.Context) error {
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

// Run scans claims immediately and periodically until ctx is canceled.
func (m *ClaimManager) Run(ctx context.Context) {
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
