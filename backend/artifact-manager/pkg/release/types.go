package release

import (
	"context"
	"time"

	"artifact-manager/pkg/repository"
)

type ReleaseState string

type FailureInfo struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	Time      time.Time `json:"time"`
}

const (
	ReleaseCreating ReleaseState = "Creating"
	ReleasePrepared ReleaseState = "Prepared"
	ReleaseReady    ReleaseState = "Ready"
	ReleaseFailed   ReleaseState = "Failed"
	ReleaseDeleting ReleaseState = "Deleting"
)

type CreateReleaseRequest struct {
	BuildName           string   `json:"buildName"`
	Project             string   `json:"project"`
	TargetOS            string   `json:"targetOS"`
	TargetArch          string   `json:"targetArch"`
	SourceRepositoryUID string   `json:"sourceRepositoryUID"`
	ExcludeSpecs        []string `json:"excludeSpecs,omitempty"`
}

type ReleaseRecord struct {
	SchemaVersion       int          `json:"schemaVersion"`
	BuildName           string       `json:"buildName"`
	Project             string       `json:"project"`
	TargetOS            string       `json:"targetOS"`
	TargetArch          string       `json:"targetArch"`
	SourceRepositoryUID string       `json:"sourceRepositoryUID"`
	ExcludeSpecs        []string     `json:"excludeSpecs,omitempty"`
	RequestDigest       string       `json:"requestDigest"`
	State               ReleaseState `json:"state"`
	Attempt             int          `json:"attempt"`
	ReleaseDigest       string       `json:"releaseDigest,omitempty"`
	ContentURL          string       `json:"contentURL,omitempty"`
	Failure             *FailureInfo `json:"failure,omitempty"`
	CreatedAt           time.Time    `json:"createdAt"`
	UpdatedAt           time.Time    `json:"updatedAt"`
	CompletedAt         *time.Time   `json:"completedAt,omitempty"`
}

type ReleaseResponse struct {
	BuildName        string       `json:"buildName"`
	State            ReleaseState `json:"state"`
	Attempt          int          `json:"attempt"`
	PollAfterSeconds int          `json:"pollAfterSeconds,omitempty"`
	ContentURL       string       `json:"contentURL,omitempty"`
	Failure          *FailureInfo `json:"failure,omitempty"`
	CreatedAt        time.Time    `json:"createdAt"`
	UpdatedAt        time.Time    `json:"updatedAt"`
	CompletedAt      *time.Time   `json:"completedAt,omitempty"`
}

type releaseIndex struct {
	SchemaVersion       int      `json:"schemaVersion"`
	BuildName           string   `json:"buildName"`
	SourceRepositoryUID string   `json:"sourceRepositoryUID"`
	RequestDigest       string   `json:"requestDigest"`
	ReleaseDigest       string   `json:"releaseDigest"`
	ExcludeSpecs        []string `json:"excludeSpecs,omitempty"`
	PublicKeySHA256     string   `json:"publicKeySHA256,omitempty"`
}

type Result struct {
	Digest string
}

type Materializer interface {
	Create(context.Context, ReleaseRecord, repository.RepositoryRecord) (Result, error)
}

type Error struct {
	Code      string
	Retryable bool
	Status    int
}

func (e *Error) Error() string {
	return e.Code
}
