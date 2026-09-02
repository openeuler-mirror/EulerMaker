package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProjectSpec   `json:"spec,omitempty"`
	Status            ProjectStatus `json:"status,omitempty"`
}

type ProjectSpec struct {
	DisplayName  string        `json:"displayName,omitempty"`
	Description  string        `json:"description,omitempty"`
	SpecBranch   string        `json:"specBranch,omitempty"`
	BuildPayload string        `json:"buildPayload,omitempty"`
	BuildTargets []BuildTarget `json:"buildTargets,omitempty"`
	PackageRepos []PackageRepo `json:"packageRepos,omitempty"`
}

type ProjectStatus struct {
	Phase           string            `json:"phase,omitempty"`
	LastBuildStatus map[string]string `json:"lastBuildStatus,omitempty"`
}

type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

type Snapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SnapshotSpec   `json:"spec,omitempty"`
	Status            SnapshotStatus `json:"status,omitempty"`
}

type SnapshotSpec struct {
	PrevSnapshot string                `json:"prevSnapshot,omitempty"`
	SpecCommits  map[string]SpecCommit `json:"specCommits,omitempty"`
	BuildTargets []BuildTarget         `json:"buildTargets,omitempty"`
}

type SnapshotStatus struct {
	Phase string `json:"phase,omitempty"`
}

type SnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Snapshot `json:"items"`
}

type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BuildSpec   `json:"spec,omitempty"`
	Status            BuildStatus `json:"status,omitempty"`
}

type BuildSpec struct {
	SnapshotName  string          `json:"snapshotName,omitempty"`
	BuildType     string          `json:"buildType,omitempty"`
	BootstrapRepo []BootstrapRepo `json:"bootstrapRepo,omitempty"`
	Packages      []string        `json:"packages,omitempty"`
	BuildTarget   BuildTarget     `json:"buildTarget,omitempty"`
	PrevBuildRepo string          `json:"prevBuildRepo,omitempty"`
}

type BootstrapRepo struct {
	Name string `json:"name,omitempty"`
	Repo string `json:"repo,omitempty"`
}

type BuildStatus struct {
	Phase      string             `json:"phase,omitempty"`
	Stage      string             `json:"stage,omitempty"`
	StartTime  metav1.Time        `json:"startTime,omitempty"`
	EndTime    metav1.Time        `json:"endTime,omitempty"`
	Repo       string             `json:"repo,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Build `json:"items"`
}

type BuildInfo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BuildInfoSpec   `json:"spec,omitempty"`
	Status            BuildInfoStatus `json:"status,omitempty"`
}

type BuildInfoSpec struct {
	SpecDepends map[string]SpecDepend `json:"specDepends,omitempty"`
}

type SpecDepend struct {
	RepoName      string                  `json:"repoName"`
	SpecName      string                  `json:"specName"`
	SpecFileName  string                  `json:"specFileName,omitempty"`
	Version       string                  `json:"version"`
	Release       string                  `json:"release,omitempty"`
	Epoch         string                  `json:"epoch,omitempty"`
	ExclusiveArch []string                `json:"exclusiveArch,omitempty"`
	Provides      []string                `json:"provides,omitempty"`
	Requires      map[string]VersionConst `json:"requires,omitempty"`
	BuildRequires map[string]VersionConst `json:"buildRequires,omitempty"`
	BuildRemoves  map[string]VersionConst `json:"buildRemoves,omitempty"`
}

type BuildInfoStatus struct {
	Phase      string                `json:"phase,omitempty"`
	Conditions []metav1.Condition    `json:"conditions,omitempty"`
	SpecStatus map[string]SpecStatus `json:"specStatus,omitempty"`
}

type SpecStatus struct {
	Build   SpecBuildStatus   `json:"build,omitempty"`
	Install SpecInstallStatus `json:"install,omitempty"`
}

type SpecBuildStatus struct {
	Status     string             `json:"status"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	JobName    string             `json:"jobName,omitempty"`
}

type SpecInstallStatus struct {
	Status      string                `json:"status"`
	MissingDeps map[string]MissingDep `json:"missingDeps,omitempty"`
	Conditions  []metav1.Condition    `json:"conditions,omitempty"`
}

type MissingDep struct {
	NeededBy        string       `json:"neededBy,omitempty"`
	VersionRequests VersionConst `json:"versionRequests,omitempty"`
}

type BuildInfoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BuildInfo `json:"items"`
}

type RpmRepo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RpmRepoSpec   `json:"spec,omitempty"`
	Status            RpmRepoStatus `json:"status,omitempty"`
}

type RpmRepoSpec struct{}

type RpmRepoStatus struct {
	Phase      string             `json:"phase,omitempty"`
	RpmDepends map[string]RpmMeta `json:"rpmDepends,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type RpmMeta struct {
	Version  string                  `json:"version"`
	SpecName string                  `json:"specName"`
	Provides map[string]string       `json:"provides,omitempty"`
	Requires map[string]VersionConst `json:"requires,omitempty"`
}

type RpmRepoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RpmRepo `json:"items"`
}

type BuildResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BuildResourceSpec `json:"spec,omitempty"`
}

type BuildResourceSpec struct {
	Default  ResourceRequirements             `json:"default,omitempty"`
	Packages map[string]PackageResourceConfig `json:"packages"`
}

type PackageResourceConfig struct {
	Default ResourceRequirements            `json:"default,omitempty"`
	Arches  map[string]ResourceRequirements `json:"arches,omitempty"`
}

type BuildResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BuildResource `json:"items"`
}

type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              JobSpec   `json:"spec,omitempty"`
	Status            JobStatus `json:"status,omitempty"`
}

type JobSpec struct {
	Priority       int64                `json:"priority,omitempty"`
	Runtime        string               `json:"runtime,omitempty"`
	RuntimeSpec    runtime.RawExtension `json:"runtimeSpec,omitempty"`
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
	Phase        string      `json:"phase,omitempty"`
	Stage        string      `json:"stage,omitempty"`
	Runner       string      `json:"runner,omitempty"`
	StartTime    metav1.Time `json:"startTime,omitempty"`
	EndTime      metav1.Time `json:"endTime,omitempty"`
	ResultRoot   string      `json:"resultRoot,omitempty"`
	Message      string      `json:"message,omitempty"`
	RestartCount int64       `json:"restartCount,omitempty"`
}

type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Job `json:"items"`
}

type Runner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RunnerSpec   `json:"spec,omitempty"`
	Status            RunnerStatus `json:"status,omitempty"`
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
	Items           []Runner `json:"items"`
}

type BuildTarget struct {
	Os          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	BuildFlag   bool   `json:"buildFlag,omitempty"`
	PublishFlag bool   `json:"publishFlag,omitempty"`
}

type PackageRepo struct {
	Name         string        `json:"name,omitempty"`
	Url          string        `json:"url,omitempty"`
	Branch       string        `json:"branch,omitempty"`
	GitTag       string        `json:"gitTag,omitempty"`
	CommitId     string        `json:"commitId,omitempty"`
	BuildTargets []BuildTarget `json:"buildTargets,omitempty"`
}

type SpecCommit struct {
	SpecUrl  string `json:"specUrl,omitempty"`
	CommitId string `json:"commitId,omitempty"`
}

type VersionConst struct {
	GT string `json:"gt,omitempty"`
	GE string `json:"ge,omitempty"`
	EQ string `json:"eq,omitempty"`
	LE string `json:"le,omitempty"`
	LT string `json:"lt,omitempty"`
}
