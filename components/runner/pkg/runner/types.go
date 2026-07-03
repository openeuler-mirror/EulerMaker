package runner

import (
	"encoding/json"
	"time"
)

type TypeMeta struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

type ObjectMeta struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
}

type RunnerResource struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta   `json:"metadata,omitempty"`
	Spec     RunnerSpec   `json:"spec,omitempty"`
	Status   RunnerStatus `json:"status,omitempty"`
}

type RunnerSpec struct {
	Type          string        `json:"type,omitempty"`
	Arch          string        `json:"arch,omitempty"`
	Hostname      string        `json:"hostname,omitempty"`
	Unschedulable bool          `json:"unschedulable,omitempty"`
	Taints        []RunnerTaint `json:"taints,omitempty"`
}

type RunnerTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

type RunnerStatus struct {
	Phase       string            `json:"phase,omitempty"`
	Conditions  []Condition       `json:"conditions,omitempty"`
	Capacity    map[string]string `json:"capacity,omitempty"`
	Allocatable map[string]string `json:"allocatable,omitempty"`
	Addresses   []RunnerAddress   `json:"addresses,omitempty"`
	Info        RunnerInfo        `json:"info,omitempty"`
	Heartbeat   *time.Time        `json:"heartbeat,omitempty"`
}

type RunnerAddress struct {
	Type    string `json:"type,omitempty"`
	Address string `json:"address,omitempty"`
}

type RunnerInfo struct {
	OS             string `json:"os,omitempty"`
	KernelVersion  string `json:"kernelVersion,omitempty"`
	Arch           string `json:"arch,omitempty"`
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
	AgentVersion   string `json:"agentVersion,omitempty"`
}

type Condition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	ObservedGeneration int64      `json:"observedGeneration,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
	Reason             string     `json:"reason,omitempty"`
	Message            string     `json:"message,omitempty"`
}

type JobResource struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta `json:"metadata,omitempty"`
	Spec     JobSpec    `json:"spec,omitempty"`
	Status   JobStatus  `json:"status,omitempty"`
}

type JobSpec struct {
	Runtime        string               `json:"runtime,omitempty"`
	RuntimeSpec    json.RawMessage      `json:"runtimeSpec,omitempty"`
	TimeoutSeconds int64                `json:"timeoutSeconds,omitempty"`
	Resources      ResourceRequirements `json:"resources,omitempty"`
	NodeSelector   map[string]string    `json:"nodeSelector,omitempty"`
	Tolerations    []Toleration         `json:"tolerations,omitempty"`
	Payload        string               `json:"payload,omitempty"`
}

type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

type JobStatus struct {
	Phase      string     `json:"phase,omitempty"`
	Stage      string     `json:"stage,omitempty"`
	Runner     string     `json:"runner,omitempty"`
	StartTime  *time.Time `json:"startTime,omitempty"`
	EndTime    *time.Time `json:"endTime,omitempty"`
	ResultRoot string     `json:"resultRoot,omitempty"`
	Message    string     `json:"message,omitempty"`
}

type WatchEvent struct {
	Type   string      `json:"type"`
	Object JobResource `json:"object"`
}
