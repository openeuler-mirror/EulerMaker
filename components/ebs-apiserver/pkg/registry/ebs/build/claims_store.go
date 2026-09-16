package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
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
	OS            string    `json:"os"`
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

type claimManager struct {
	backend coordinationBackend
	builds  rest.Getter
	list    rest.Lister
	now     func() time.Time
}

func newClaimManager(backend coordinationBackend, builds rest.Getter, list rest.Lister) *claimManager {
	registerClaimMetrics()
	return &claimManager{backend: backend, builds: builds, list: list, now: time.Now}
}

func coordinationID(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c claim) id() string { return coordinationID(c.Project, c.OS, c.Arch) }

func coordinationDocument(id, project, kind string, created time.Time, value interface{}) es.Document {
	data, _ := json.Marshal(value) // Only the concrete string/time records above are used.
	return es.Document{APIVersion: "internal/v1", Kind: kind, DocumentID: id,
		Metadata: es.Metadata{Name: id, Namespace: project, CreationTimestamp: created.Format(time.RFC3339Nano)}, Data: data}
}

func (c claim) document() es.Document {
	return coordinationDocument(c.id(), c.Project, "BuildTargetClaim", c.CreatedAt, c)
}

func (m *claimManager) get(ctx context.Context, id string) (*claimRecord, error) {
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
func (m *claimManager) transition(ctx context.Context, c *claimRecord, state, reason string) (*claimRecord, error) {
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

func (m *claimManager) release(ctx context.Context, c *claimRecord, reason string) error {
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

func (m *claimManager) getBuild(ctx context.Context, c claim) (*ebsv1.Build, error) {
	obj, err := m.builds.Get(request.WithNamespace(ctx, c.Project), c.BuildName, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	b, ok := obj.(*ebsv1.Build)
	if !ok || b == nil {
		return nil, fmt.Errorf("expected Build, got %T", obj)
	}
	if b.Namespace != c.Project || b.Name != c.BuildName || b.Spec.BuildTarget.Os != c.OS || b.Spec.BuildTarget.Arch != c.Arch || !limitedBuild(b) {
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
