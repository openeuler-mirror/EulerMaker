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
	JobUID    string    `json:"jobUID,omitempty"`
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
