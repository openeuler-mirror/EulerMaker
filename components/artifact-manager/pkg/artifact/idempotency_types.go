package artifact

import "time"

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
