package framework

import (
	"context"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	"k8s.io/apimachinery/pkg/types"
)

type RunnerSnapshot struct {
	Runner          *ebsv1.Runner
	Allocatable     Resource
	Available       Resource
	RunningJobCount int64
	AssumedJobCount int64
	Revision        uint64
	Invalid         error
}

type Snapshot struct {
	Job       *ebsv1.Job
	Requests  Resource
	Runners   map[string]*RunnerSnapshot
	CreatedAt time.Time
	Invalid   error
}

type Session struct {
	CycleID       string
	Job           *ebsv1.Job
	Requests      Resource
	Runners       map[string]*RunnerSnapshot
	OpenedAt      time.Time
	FilterPlugins []FilterPlugin
	ScorePlugins  []ScorePlugin
}

type StatusCode string

const (
	Success       StatusCode = "Success"
	Unschedulable StatusCode = "Unschedulable"
	Error         StatusCode = "Error"
)

type Status struct {
	Code   StatusCode
	Plugin string
	Reason string
	Err    error
}

func SuccessStatus(plugin string) *Status { return &Status{Code: Success, Plugin: plugin} }

type FilterPlugin interface {
	Name() string
	Filter(context.Context, *Session, *RunnerSnapshot) *Status
}

type ScorePlugin interface {
	Name() string
	Score(context.Context, *Session, *RunnerSnapshot) (int64, *Status)
	Weight() int64
}

type Action interface {
	Name() string
	Execute(context.Context, *Session) *CycleResult
}

type QueueAction string

const (
	QueueDone       QueueAction = "Done"
	QueueAddBackoff QueueAction = "AddBackoff"
)

type ResultCode string

const (
	Scheduled           ResultCode = "Scheduled"
	ResultUnschedulable ResultCode = "Unschedulable"
	UnschedulableError  ResultCode = "UnschedulableError"
	Conflict            ResultCode = "Conflict"
	InternalError       ResultCode = "InternalError"
	BindUnknown         ResultCode = "BindUnknown"
)

type CycleResult struct {
	Code        ResultCode
	JobKey      string
	JobUID      types.UID
	RunnerName  string
	Reason      string
	Err         error
	QueueAction QueueAction
}
