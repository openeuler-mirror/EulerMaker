package es

type OldJobDocument struct {
	Document
	Archive *JobArchiveRecord `json:"archive,omitempty"`
}

// JobArchiveRecord fences create-only archive requests even after the public OldJob is removed.
type JobArchiveRecord struct {
	UID     string `json:"uid"`
	Digest  string `json:"digest"`
	Action  string `json:"action"`
	Deleted bool   `json:"deleted"`
}
