package artifact

type APIError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	RequestID string         `json:"requestID"`
	Details   map[string]any `json:"details,omitempty"`
}
