package artifact

import "time"

type ManifestState string

const (
	ManifestOpen       ManifestState = "Open"
	ManifestCompleting ManifestState = "Completing"
	ManifestCompleted  ManifestState = "Completed"
	ManifestFailed     ManifestState = "Failed"
)

type ManifestFile struct {
	ArtifactID   string   `json:"artifactID"`
	RelativePath string   `json:"relativePath"`
	Category     Category `json:"category"`
	Size         int64    `json:"size"`
	SHA256       string   `json:"sha256"`
	Required     bool     `json:"required"`
}
type JobUploadManifest struct {
	SchemaVersion int            `json:"schemaVersion"`
	Project       string         `json:"project"`
	JobName       string         `json:"jobName"`
	JobUID        string         `json:"jobUID"`
	RunnerName    string         `json:"runnerName"`
	Files         []ManifestFile `json:"files"`
	Digest        string         `json:"digest,omitempty"`
	State         ManifestState  `json:"state"`
	Failure       *FailureInfo   `json:"failure,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	CompletedAt   *time.Time     `json:"completedAt,omitempty"`
	ExpiresAt     *time.Time     `json:"expiresAt,omitempty"`
}
