package repository

import (
	"sync"
	"sync/atomic"
	"time"
)

type Action string

const (
	ActionSync   Action = "Sync"
	ActionDelete Action = "Delete"
)

type RepositoryError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type counters struct {
	active           atomic.Int64
	syncSuccess      atomic.Uint64
	syncFailure      atomic.Uint64
	deleteSuccess    atomic.Uint64
	deleteFailure    atomic.Uint64
	retries          atomic.Uint64
	syncDurationNS   atomic.Uint64
	syncDurationN    atomic.Uint64
	deleteDurationNS atomic.Uint64
	deleteDurationN  atomic.Uint64
}

type Metrics struct {
	QueueDepth            int
	ActiveWorkers         int64
	Repositories          int
	SyncSuccess           uint64
	SyncFailure           uint64
	DeleteSuccess         uint64
	DeleteFailure         uint64
	Retries               uint64
	SyncDurationSeconds   float64
	SyncDurationCount     uint64
	DeleteDurationSeconds float64
	DeleteDurationCount   uint64
}

type Response struct {
	Key        string           `json:"key"`
	CloneURL   string           `json:"clone_url,omitempty"`
	SyncTime   *time.Time       `json:"sync_time,omitempty"`
	RetryCount int              `json:"retry_count,omitempty"`
	Error      *RepositoryError `json:"error,omitempty"`
}

type state struct {
	Key           string
	OriginURL     string
	DesiredAction Action
	Revision      uint64
	Available     bool
	SyncTime      *time.Time
	RetryCount    int
	Error         *RepositoryError
	ResetBackoff  bool
	operationLock sync.RWMutex
}

type snapshot struct {
	action    Action
	originURL string
	revision  uint64
	reset     bool
}
