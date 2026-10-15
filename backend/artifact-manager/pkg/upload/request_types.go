package upload

type UploadMetadata struct {
	Category     Category `json:"category"`
	FileName     string   `json:"fileName"`
	RelativePath string   `json:"relativePath"`
	ContentType  string   `json:"contentType,omitempty"`
	Size         int64    `json:"size"`
	SHA256       string   `json:"sha256"`
}

type CompleteManifestRequest struct {
	Files []ManifestFile `json:"files"`
}

type CompleteLogRequest struct {
	Stream       string `json:"stream"`
	LastSequence int64  `json:"lastSequence"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}
