package artifact

import "time"

type Category string

const (
	CategoryArtifact Category = "artifact"
	CategoryLog      Category = "log"
)

type State string

const (
	Pending   State = "Pending"
	Uploading State = "Uploading"
	Completed State = "Completed"
	Failed    State = "Failed"
	Expired   State = "Expired"
)

type FailureInfo struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	Time      time.Time `json:"time"`
}
type Artifact struct {
	SchemaVersion int          `json:"schemaVersion"`
	ID            string       `json:"id"`
	Project       string       `json:"project"`
	JobName       string       `json:"jobName"`
	JobUID        string       `json:"jobUID"`
	RunnerName    string       `json:"runnerName"`
	Category      Category     `json:"category"`
	Name          string       `json:"name,omitempty"`
	FileName      string       `json:"fileName"`
	RelativePath  string       `json:"relativePath"`
	ContentType   string       `json:"contentType,omitempty"`
	Size          int64        `json:"size"`
	SHA256        string       `json:"sha256"`
	StorageKey    string       `json:"storageKey"`
	State         State        `json:"state"`
	Failure       *FailureInfo `json:"failure,omitempty"`
	CreatedAt     time.Time    `json:"createdAt"`
	UpdatedAt     time.Time    `json:"updatedAt"`
	CompletedAt   *time.Time   `json:"completedAt,omitempty"`
	ExpiresAt     *time.Time   `json:"expiresAt,omitempty"`
}
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
	SchemaVersion  int            `json:"schemaVersion"`
	Project        string         `json:"project"`
	JobName        string         `json:"jobName"`
	JobUID         string         `json:"jobUID"`
	RunnerName     string         `json:"runnerName"`
	Generation     int64          `json:"generation"`
	IdempotencyKey string         `json:"idempotencyKey"`
	Files          []ManifestFile `json:"files"`
	Digest         string         `json:"digest,omitempty"`
	State          ManifestState  `json:"state"`
	Failure        *FailureInfo   `json:"failure,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	CompletedAt    *time.Time     `json:"completedAt,omitempty"`
	ExpiresAt      *time.Time     `json:"expiresAt,omitempty"`
}
type IdempotencyState string

const (
	IdempotencyProcessing IdempotencyState = "Processing"
	IdempotencyCompleted  IdempotencyState = "Completed"
	IdempotencyFailed     IdempotencyState = "Failed"
)

type IdempotencyRecord struct {
	SchemaVersion int              `json:"schemaVersion"`
	Scope         string           `json:"scope"`
	Key           string           `json:"key"`
	RequestDigest string           `json:"requestDigest"`
	ArtifactID    string           `json:"artifactID"`
	State         IdempotencyState `json:"state"`
	Failure       *FailureInfo     `json:"failure,omitempty"`
	CreatedAt     time.Time        `json:"createdAt"`
	UpdatedAt     time.Time        `json:"updatedAt"`
	CompletedAt   *time.Time       `json:"completedAt,omitempty"`
	ExpiresAt     *time.Time       `json:"expiresAt,omitempty"`
}
type LogState string

const (
	LogOpen       LogState = "Open"
	LogFinalizing LogState = "Finalizing"
	LogCompleted  LogState = "Completed"
	LogFailed     LogState = "Failed"
	LogExpired    LogState = "Expired"
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
	JobUID         string       `json:"jobUID"`
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
	ExpiresAt      *time.Time   `json:"expiresAt,omitempty"`
}

type UploadMetadata struct {
	JobUID       string   `json:"jobUID"`
	Category     Category `json:"category"`
	Name         string   `json:"name,omitempty"`
	FileName     string   `json:"fileName"`
	RelativePath string   `json:"relativePath"`
	ContentType  string   `json:"contentType,omitempty"`
	Size         int64    `json:"size"`
	SHA256       string   `json:"sha256"`
}
type CompleteManifestRequest struct {
	JobUID     string         `json:"jobUID"`
	Generation int64          `json:"generation"`
	Files      []ManifestFile `json:"files"`
}
type CompleteLogRequest struct {
	JobUID       string `json:"jobUID"`
	Stream       string `json:"stream"`
	LastSequence int64  `json:"lastSequence"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}
type APIError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	RequestID string         `json:"requestID"`
	Details   map[string]any `json:"details,omitempty"`
}
