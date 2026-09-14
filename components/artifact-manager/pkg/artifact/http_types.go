package artifact

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
	JobUID string         `json:"jobUID"`
	Files  []ManifestFile `json:"files"`
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
