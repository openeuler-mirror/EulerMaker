package artifact

import "time"

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
