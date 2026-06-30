package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSpec   `json:"spec,omitempty"`
	Status ProjectStatus `json:"status,omitempty"`
}

type ProjectSpec struct {
	DisplayName    string        `json:"displayName,omitempty"`
	Description    string        `json:"description,omitempty"`
	SpecBranch     string        `json:"specBranch,omitempty"`
	BuildEnvMacros string        `json:"buildEnvMacros,omitempty"`
	BuildTargets   []BuildTarget `json:"buildTargets,omitempty"`
	PackageRepos   []PackageRepo `json:"packageRepos,omitempty"`
}

type ProjectStatus struct {
	Phase         string             `json:"phase,omitempty"`
	SnapshotCount int32              `json:"snapshotCount,omitempty"`
	BuildCount    int32              `json:"buildCount,omitempty"`
	LastBuildTime metav1.Time        `json:"lastBuildTime,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}

type BuildTarget struct {
	OsVariant      string           `json:"osVariant,omitempty"`
	Architecture   string           `json:"architecture,omitempty"`
	GroundProjects []string         `json:"groundProjects,omitempty"`
	Flags          BuildTargetFlags `json:"flags,omitempty"`
}

type BuildTargetFlags struct {
	Build   bool `json:"build,omitempty"`
	Publish bool `json:"publish,omitempty"`
}

type PackageRepo struct {
	SpecName   string `json:"specName,omitempty"`
	SpecUrl    string `json:"specUrl,omitempty"`
	SpecBranch string `json:"specBranch,omitempty"`
	GitTag     string `json:"gitTag,omitempty"`
	CommitId   string `json:"commitId,omitempty"`
}

type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Project `json:"items"`
}

type Snapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SnapshotSpec   `json:"spec,omitempty"`
	Status SnapshotStatus `json:"status,omitempty"`
}

type SnapshotSpec struct {
	IsTrunk        bool                  `json:"isTrunk,omitempty"`
	PrevSnapshot   string                `json:"prevSnapshot,omitempty"`
	SpecCommits    map[string]SpecCommit `json:"specCommits,omitempty"`
	BuildTargets   []BuildTarget         `json:"buildTargets,omitempty"`
	GroundProjects map[string]string     `json:"groundProjects,omitempty"`
}

type SnapshotStatus struct {
	Phase     string      `json:"phase,omitempty"`
	BuildId   string      `json:"buildId,omitempty"`
	StartTime metav1.Time `json:"startTime,omitempty"`
}

type SpecCommit struct {
	SpecUrl    string `json:"specUrl,omitempty"`
	SpecBranch string `json:"specBranch,omitempty"`
	CommitId   string `json:"commitId,omitempty"`
	CommitTime string `json:"commitTime,omitempty"`
	GitRepo    string `json:"gitRepo,omitempty"`
}

type SnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Snapshot `json:"items"`
}

type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BuildSpec   `json:"spec,omitempty"`
	Status BuildStatus `json:"status,omitempty"`
}

type BuildSpec struct {
	SnapshotName string      `json:"snapshotName,omitempty"`
	BuildType    string      `json:"buildType,omitempty"`
	BuildTarget  BuildTarget `json:"buildTarget,omitempty"`
	Packages     []string    `json:"packages,omitempty"`
}

type BuildStatus struct {
	Phase         string                   `json:"phase,omitempty"`
	StartTime     metav1.Time              `json:"startTime,omitempty"`
	EndTime       metav1.Time              `json:"endTime,omitempty"`
	RepoId        string                   `json:"repoId,omitempty"`
	PackageStatus map[string]PackageStatus `json:"packageStatus,omitempty"`
	Conditions    []metav1.Condition       `json:"conditions,omitempty"`
}

type PackageStatus struct {
	Phase     string      `json:"phase,omitempty"`
	JobId     string      `json:"jobId,omitempty"`
	StartTime metav1.Time `json:"startTime,omitempty"`
	EndTime   metav1.Time `json:"endTime,omitempty"`
	Attempts  int32       `json:"attempts,omitempty"`
	Message   string      `json:"message,omitempty"`
}

type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Build `json:"items"`
}

type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JobSpec   `json:"spec,omitempty"`
	Status JobStatus `json:"status,omitempty"`
}

type JobSpec struct {
	Runner      string               `json:"runner,omitempty"`
	Arch        string               `json:"arch,omitempty"`
	Runtime     int64                `json:"runtime,omitempty"`
	DockerImage string               `json:"dockerImage,omitempty"`
	RepoUrl     string               `json:"repoUrl,omitempty"`
	Package     string               `json:"package,omitempty"`
	ImageConfig runtime.RawExtension `json:"imageConfig,omitempty"`
	Env         map[string]string    `json:"env,omitempty"`
	Commands    []string             `json:"commands,omitempty"`
}

type JobStatus struct {
	Phase      string      `json:"phase,omitempty"`
	Stage      string      `json:"stage,omitempty"`
	Runner     string      `json:"runner,omitempty"`
	StartTime  metav1.Time `json:"startTime,omitempty"`
	EndTime    metav1.Time `json:"endTime,omitempty"`
	ResultRoot string      `json:"resultRoot,omitempty"`
	Message    string      `json:"message,omitempty"`
}

type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Job `json:"items"`
}

type Runner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunnerSpec   `json:"spec,omitempty"`
	Status RunnerStatus `json:"status,omitempty"`
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
	Phase       string             `json:"phase,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
	Capacity    map[string]string  `json:"capacity,omitempty"`
	Allocatable map[string]string  `json:"allocatable,omitempty"`
	Addresses   []RunnerAddress    `json:"addresses,omitempty"`
	Info        RunnerInfo         `json:"info,omitempty"`
	Heartbeat   metav1.Time        `json:"heartbeat,omitempty"`
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

type RunnerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Runner `json:"items"`
}
