package artifact

import (
	"context"
	"time"
)

type RepositoryState string

const (
	RepositoryCreating RepositoryState = "Creating"
	RepositoryReady    RepositoryState = "Ready"
	RepositoryFailed   RepositoryState = "Failed"
	RepositoryDeleting RepositoryState = "Deleting"
)

type ManifestReference struct {
	JobName string `json:"jobName"`
	JobUID  string `json:"jobUID"`
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
	SchemaVersion     int                          `json:"schemaVersion"`
	RepositoryUID     string                       `json:"repositoryUID"`
	RepositoryName    string                       `json:"repositoryName"`
	Project           string                       `json:"project"`
	BuildName         string                       `json:"buildName"`
	TargetOS          string                       `json:"targetOS"`
	TargetArch        string                       `json:"targetArch"`
	BaseRepositoryUID string                       `json:"baseRepositoryUID,omitempty"`
	Manifests         []ManifestReference          `json:"manifests"`
	RequestDigest     string                       `json:"requestDigest"`
	State             RepositoryState              `json:"state"`
	Attempt           int                          `json:"attempt"`
	PackageCount      int                          `json:"packageCount,omitempty"`
	RepositoryDigest  string                       `json:"repositoryDigest,omitempty"`
	ContentURL        string                       `json:"contentURL,omitempty"`
	RPMs              map[string]RepositoryRPMMeta `json:"rpms,omitempty"`
	Failure           *FailureInfo                 `json:"failure,omitempty"`
	CreatedAt         time.Time                    `json:"createdAt"`
	UpdatedAt         time.Time                    `json:"updatedAt"`
	CompletedAt       *time.Time                   `json:"completedAt,omitempty"`
	baseBuildName     string
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
	RepositoryUID    string                       `json:"repositoryUID"`
	State            RepositoryState              `json:"state"`
	Attempt          int                          `json:"attempt"`
	PollAfterSeconds int                          `json:"pollAfterSeconds,omitempty"`
	ContentURL       string                       `json:"contentURL,omitempty"`
	RepositoryDigest string                       `json:"repositoryDigest,omitempty"`
	PackageCount     int                          `json:"packageCount,omitempty"`
	RPMs             map[string]RepositoryRPMMeta `json:"rpms,omitempty"`
	Failure          *FailureInfo                 `json:"failure,omitempty"`
	CreatedAt        time.Time                    `json:"createdAt"`
	UpdatedAt        time.Time                    `json:"updatedAt"`
	CompletedAt      *time.Time                   `json:"completedAt,omitempty"`
}

type repositoryResult struct {
	Digest string
	Count  int
	RPMs   map[string]RepositoryRPMMeta
}

type repositoryIndex struct {
	SchemaVersion    int                          `json:"schemaVersion"`
	RepositoryUID    string                       `json:"repositoryUID"`
	RequestDigest    string                       `json:"requestDigest"`
	RepositoryDigest string                       `json:"repositoryDigest"`
	RPMs             map[string]RepositoryRPMMeta `json:"rpms"`
}

type repositoryMaterializer interface {
	Materialize(context.Context, RepositoryRecord) (repositoryResult, error)
}

type repositoryError struct {
	code      string
	retryable bool
	status    int
}

func (e *repositoryError) Error() string { return e.code }
