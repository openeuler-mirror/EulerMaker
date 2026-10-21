package upload

import "time"

type Category string

const (
	CategoryArtifact Category = "artifact"
	CategoryLog      Category = "log"
)

type State string

const (
	Pending   State = "Pending"
	Completed State = "Completed"
	Failed    State = "Failed"
)

type FailureInfo struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	Time      time.Time `json:"time"`
}

type Artifact struct {
	SchemaVersion  int          `json:"schemaVersion"`
	ID             string       `json:"id"`
	Category       Category     `json:"category"`
	FileName       string       `json:"fileName"`
	RelativePath   string       `json:"relativePath"`
	ContentType    string       `json:"contentType,omitempty"`
	Size           int64        `json:"size"`
	SHA256         string       `json:"sha256"`
	StorageKey     string       `json:"storageKey"`
	IdempotencyKey string       `json:"idempotencyKey,omitempty"`
	State          State        `json:"state"`
	Failure        *FailureInfo `json:"failure,omitempty"`
	CreatedAt      time.Time    `json:"createdAt"`
	UpdatedAt      time.Time    `json:"updatedAt"`
	CompletedAt    *time.Time   `json:"completedAt,omitempty"`
}

type ManifestState string

const ManifestCompleted ManifestState = "Completed"

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

type LogState string

const (
	LogOpen       LogState = "Open"
	LogFinalizing LogState = "Finalizing"
	LogCompleted  LogState = "Completed"
	LogFailed     LogState = "Failed"
)

type LogChunkRecord struct {
	Sequence    int64  `json:"sequence"`
	StartOffset int64  `json:"startOffset"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

type LogStream struct {
	SchemaVersion  int          `json:"schemaVersion"`
	Project        string       `json:"project"`
	JobName        string       `json:"jobName"`
	RunnerName     string       `json:"runnerName"`
	Stream         string       `json:"stream"`
	State          LogState     `json:"state"`
	NextSequence   int64        `json:"nextSequence"`
	CommittedBytes int64        `json:"committedBytes"`
	ArtifactID     string       `json:"artifactID,omitempty"`
	FinalSize      *int64       `json:"finalSize,omitempty"`
	FinalSHA256    string       `json:"finalSHA256,omitempty"`
	Failure        *FailureInfo `json:"failure,omitempty"`
	CreatedAt      time.Time    `json:"createdAt"`
	UpdatedAt      time.Time    `json:"updatedAt"`
	CompletedAt    *time.Time   `json:"completedAt,omitempty"`
}
