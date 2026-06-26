package runner

import "time"

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
	RunningJobs []string          `json:"runningJobs,omitempty"`
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
	Runner      string            `json:"runner,omitempty"`
	Arch        string            `json:"arch,omitempty"`
	Runtime     int64             `json:"runtime,omitempty"`
	DockerImage string            `json:"dockerImage,omitempty"`
	RepoURL     string            `json:"repoUrl,omitempty"`
	Package     string            `json:"package,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Commands    []string          `json:"commands,omitempty"`
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
