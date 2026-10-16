package repository

import (
	"context"
	"time"

	"artifact-manager/pkg/upload"
)

type RepositoryState string

type Config struct {
	CreateRepoCommand string
	RPMQueryCommand   string
	CreateRepoWorkers int
	Timeout           time.Duration
	WorkTTL           time.Duration
	Workers           int
	QueueCapacity     int
}

const (
	RepositoryCreating RepositoryState = "Creating"
	RepositoryReady    RepositoryState = "Ready"
	RepositoryFailed   RepositoryState = "Failed"
	RepositoryDeleting RepositoryState = "Deleting"
)

type ManifestReference struct {
	JobName string `json:"jobName"`
}

type CreateRepositoryRequest struct {
	RepositoryUID     string              `json:"repositoryUID"`
	RepositoryName    string              `json:"repositoryName"`
	Project           string              `json:"project"`
	BuildName         string              `json:"buildName"`
	TargetOS          string              `json:"targetOS"`
	TargetArch        string              `json:"targetArch"`
	BaseRepositoryUID string              `json:"baseRepositoryUID,omitempty"`
	Manifests         []ManifestReference `json:"manifests"`
}

type RepositoryRecord struct {
	SchemaVersion      int                          `json:"schemaVersion"`
	RepositoryUID      string                       `json:"repositoryUID"`
	RepositoryName     string                       `json:"repositoryName"`
	Project            string                       `json:"project"`
	BuildName          string                       `json:"buildName"`
	TargetOS           string                       `json:"targetOS"`
	TargetArch         string                       `json:"targetArch"`
	BaseRepositoryUID  string                       `json:"baseRepositoryUID,omitempty"`
	Manifests          []ManifestReference          `json:"manifests"`
	RequestDigest      string                       `json:"requestDigest"`
	SigningFingerprint string                       `json:"signingFingerprint,omitempty"`
	State              RepositoryState              `json:"state"`
	Attempt            int                          `json:"attempt"`
	RepositoryDigest   string                       `json:"repositoryDigest,omitempty"`
	ContentURL         string                       `json:"contentURL,omitempty"`
	RPMs               map[string]RepositoryRPMMeta `json:"rpms,omitempty"`
	Failure            *upload.FailureInfo          `json:"failure,omitempty"`
	CreatedAt          time.Time                    `json:"createdAt"`
	UpdatedAt          time.Time                    `json:"updatedAt"`
	CompletedAt        *time.Time                   `json:"completedAt,omitempty"`
	baseBuildName      string
}

type RepositoryRPMMeta struct {
	FileName string   `json:"fileName"`
	SHA256   string   `json:"sha256"`
	Size     int64    `json:"size"`
	Name     string   `json:"name"`
	Epoch    string   `json:"epoch,omitempty"`
	Version  string   `json:"version"`
	Release  string   `json:"release"`
	Arch     string   `json:"arch"`
	Source   string   `json:"sourceRPM,omitempty"`
	SpecName string   `json:"specName"`
	Provides []string `json:"provides,omitempty"`
	Requires []string `json:"requires,omitempty"`
}

type RepositoryResponse struct {
	RepositoryUID    string              `json:"repositoryUID"`
	State            RepositoryState     `json:"state"`
	Attempt          int                 `json:"attempt"`
	PollAfterSeconds int                 `json:"pollAfterSeconds,omitempty"`
	ContentURL       string              `json:"contentURL,omitempty"`
	Failure          *upload.FailureInfo `json:"failure,omitempty"`
	CreatedAt        time.Time           `json:"createdAt"`
	UpdatedAt        time.Time           `json:"updatedAt"`
	CompletedAt      *time.Time          `json:"completedAt,omitempty"`
}

type Result struct {
	Digest string
}

type Index struct {
	SchemaVersion      int                          `json:"schemaVersion"`
	RepositoryUID      string                       `json:"repositoryUID"`
	RequestDigest      string                       `json:"requestDigest"`
	SigningFingerprint string                       `json:"signingFingerprint,omitempty"`
	RepositoryDigest   string                       `json:"repositoryDigest"`
	RPMs               map[string]RepositoryRPMMeta `json:"rpms"`
}

type Materializer interface {
	Materialize(context.Context, RepositoryRecord) (Result, error)
}

type Error struct {
	Code      string
	Retryable bool
	Status    int
	JobName   string
}

func (e *Error) Error() string {
	return e.Code
}
