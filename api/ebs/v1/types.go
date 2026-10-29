package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:deepcopy-gen=true
// Config is a cluster-scoped, versioned text configuration. Consumers interpret
// Content according to the object's name; the API parses and validates built-in configurations.
type Config struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ConfigSpec `json:"spec"`
}

type ConfigSpec struct {
	Visibility ConfigVisibility `json:"visibility"`
	Content    string           `json:"content"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:deepcopy-gen=true
type ConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Config `json:"items"`
}

type ConfigVisibility string

const (
	ConfigVisibilityPublic  ConfigVisibility = "Public"
	ConfigVisibilityOpsOnly ConfigVisibility = "OpsOnly"
	BuildTargetConfigName                    = "build-target"
	BuildResourceConfigName                  = "build-resource"
	ServiceConfigName                        = "services"
)

// +k8s:deepcopy-gen=true
type BuildTargetContent struct {
	Targets map[string]BuildTargetConfigEntry `json:"targets"`
}

// +k8s:deepcopy-gen=true
type BuildTargetConfigEntry struct {
	Arches map[string]BuildTargetArch `json:"arches"`
}

// +k8s:deepcopy-gen=true
type BuildTargetArch struct {
	Image string `json:"image"`
}

type BuildResourceContent struct {
	Default  ResourceRequirements             `json:"default,omitempty"`
	Packages map[string]PackageResourceConfig `json:"packages"`
}

type PackageResourceConfig struct {
	Default ResourceRequirements            `json:"default,omitempty"`
	Arches  map[string]ResourceRequirements `json:"arches,omitempty"`
}

// ServiceConfigContent defines the service endpoints in Config/services.
// +k8s:deepcopy-gen=true
type ServiceConfigContent struct {
	ArtifactManagers map[string]ServiceEndpoints `json:"artifactManagers" yaml:"artifactManagers"`
	GitServer        ServiceEndpoints            `json:"gitServer" yaml:"gitServer"`
}

// +k8s:deepcopy-gen=true
type ServiceEndpoints struct {
	InternalURL       string `json:"internalURL" yaml:"internalURL"`
	RunnerInternalURL string `json:"runnerInternalURL,omitempty" yaml:"runnerInternalURL,omitempty"`
	RunnerExternalURL string `json:"runnerExternalURL,omitempty" yaml:"runnerExternalURL,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:deepcopy-gen=true
type Script struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ScriptSpec `json:"spec"`
}

// +k8s:deepcopy-gen=true
type ScriptSpec struct {
	Content string `json:"content"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:deepcopy-gen=true
type ScriptList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Script `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProjectSpec   `json:"spec,omitempty"`
	Status            ProjectStatus `json:"status,omitempty"`
}

type ProjectSpec struct {
	DisplayName    string          `json:"displayName,omitempty"`
	Description    string          `json:"description,omitempty"`
	DefaultRef     string          `json:"defaultRef,omitempty"`
	BuildPayload   string          `json:"buildPayload,omitempty"`
	BuildTargets   []BuildTarget   `json:"buildTargets,omitempty"`
	PackageRepos   []PackageRepo   `json:"packageRepos,omitempty"`
	BootstrapRepos []BootstrapRepo `json:"bootstrapRepos,omitempty"`
}

type ProjectStatus struct {
	Phase ProjectPhase `json:"phase,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Snapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SnapshotSpec   `json:"spec,omitempty"`
	Status            SnapshotStatus `json:"status,omitempty"`
}

type SnapshotSpec struct {
	DefaultRef   string        `json:"defaultRef,omitempty"`
	PackageRepos []PackageRepo `json:"packageRepos,omitempty"`
}

type SnapshotStatus struct {
	Phase               SnapshotPhase                `json:"phase,omitempty"`
	PackageRepoStatuses map[string]PackageRepoStatus `json:"packageRepoStatuses,omitempty"`
	Conditions          []metav1.Condition           `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Snapshot `json:"items"`
}

type PackageRepoStatus struct {
	CloneURL string            `json:"cloneURL,omitempty"`
	CommitID string            `json:"commitID,omitempty"`
	Error    *PackageRepoError `json:"error,omitempty"`
}

type PackageRepoError struct {
	Code      PackageRepoErrorCode `json:"code,omitempty"`
	Message   string               `json:"message,omitempty"`
	Retryable bool                 `json:"retryable,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BuildSpec   `json:"spec,omitempty"`
	Status            BuildStatus `json:"status,omitempty"`
}

type BuildSpec struct {
	BuildType   BuildType   `json:"buildType,omitempty"`
	BuildTarget BuildTarget `json:"buildTarget,omitempty"`
	// Publish controls release publication. Defaults to true for non-single builds; single builds must omit it.
	Publish  *bool    `json:"publish,omitempty"`
	Packages []string `json:"packages,omitempty"`
}

type BuildStatus struct {
	Phase        BuildPhase         `json:"phase,omitempty"`
	Stage        BuildStage         `json:"stage,omitempty"`
	StartTime    metav1.Time        `json:"startTime,omitempty"`
	EndTime      metav1.Time        `json:"endTime,omitempty"`
	BaseBuildRef *BaseBuildRef      `json:"baseBuildRef,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Build `json:"items"`
}

type BaseBuildRef struct {
	Name string `json:"name,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BuildInfo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BuildInfoSpec   `json:"spec,omitempty"`
	Status            BuildInfoStatus `json:"status,omitempty"`
}

type BuildInfoSpec struct {
	BuildPayload   string          `json:"buildPayload,omitempty"`
	BootstrapRepos []BootstrapRepo `json:"bootstrapRepos,omitempty"`
}

// BuildInfoStatus records preparation, build and installation results by repository/file identity.
type BuildInfoStatus struct {
	Phase       BuildInfoPhase     `json:"phase,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
	SpecResults SpecResults        `json:"specResults"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BuildInfoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BuildInfo `json:"items"`
}

// SpecResults keys are repository/root-spec-filename; repository/ records a failure before spec discovery.
type SpecResults map[string]SpecResult

// SpecResult uses one verdict for the complete preparation/build/install flow.
// SpecName is required after successful parsing and may be empty if parsing fails or dispatch stops before parsing.
type SpecResult struct {
	SpecName      string                `json:"specName,omitempty"`
	Status        SpecResultPhase       `json:"status"`
	Message       string                `json:"message,omitempty"`
	MissingDeps   map[string]MissingDep `json:"missingDeps,omitempty"`
	DispatchCount int64                 `json:"dispatchCount,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// BuildGraph persists the dependency graph owned by the same-name BuildInfo.
type BuildGraph struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Status            BuildGraphStatus `json:"status"`
}

type BuildGraphStatus struct {
	Nodes map[string]BuildGraphNode `json:"nodes"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BuildGraphList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BuildGraph `json:"items"`
}

// BuildGraphNode records one spec's dependency edges. OutDep lists downstream
// specs that depend on this spec; InDep/InstallInDep map
// upstream specs to the matched version constraints. BootstrapBreak marks
// a cycle-break node; it is persisted and never re-selected on load.
type BuildGraphNode struct {
	Version        string                       `json:"version,omitempty"`
	OutDep         []string                     `json:"outDep,omitempty"`
	InDep          map[string]VersionConstraint `json:"inDep,omitempty"`
	InstallInDep   map[string]VersionConstraint `json:"installInDep,omitempty"`
	BootstrapBreak bool                         `json:"bootstrapBreak,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type RpmRepo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Status            RpmRepoStatus `json:"status,omitempty"`
}

type RpmRepoStatus struct {
	Repository *RpmRepoRepositoryStatus `json:"repository,omitempty"`
	Release    *RpmRepoReleaseStatus    `json:"release,omitempty"`
	Conditions []metav1.Condition       `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type RpmRepoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RpmRepo `json:"items"`
}

type RpmRepoRepositoryStatus struct {
	RepositoryUID   string                `json:"repositoryUID,omitempty"`
	ContentURL      string                `json:"contentURL,omitempty"`
	SourceJobNames  []string              `json:"sourceJobNames,omitempty"`
	SkippedJobNames []string              `json:"skippedJobNames,omitempty"`
	Transition      *RepositoryTransition `json:"transition,omitempty"`
	UpdatedAt       *metav1.Time          `json:"updatedAt,omitempty"`
}

type RepositoryTransition struct {
	Inputs            []RepositoryInput `json:"inputs"`
	BaseRepositoryUID string            `json:"baseRepositoryUID,omitempty"`
	RepositoryUID     string            `json:"repositoryUID"`
}

type RepositoryInput struct {
	JobName  string `json:"jobName"`
	SpecName string `json:"specName"`
}

type RpmRepoReleaseStatus struct {
	Phase               RpmRepoReleasePhase `json:"phase,omitempty"`
	SourceRepositoryUID string              `json:"sourceRepositoryUID,omitempty"`
	ContentURL          string              `json:"contentURL,omitempty"`
	Transition          *ReleaseTransition  `json:"transition,omitempty"`
	UpdatedAt           *metav1.Time        `json:"updatedAt,omitempty"`
}

type ReleaseTransition struct {
	SourceRepositoryUID string   `json:"sourceRepositoryUID"`
	ExcludeSpecs        []string `json:"excludeSpecs,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              JobSpec   `json:"spec,omitempty"`
	Status            JobStatus `json:"status,omitempty"`
}

type JobSpec struct {
	Priority       int64                `json:"priority,omitempty"`
	Runtime        ExecutionType        `json:"runtime,omitempty"`
	RuntimeSpec    runtime.RawExtension `json:"runtimeSpec,omitempty"`
	ScriptRefs     []ScriptRef          `json:"scriptRefs,omitempty"`
	TimeoutSeconds int64                `json:"timeoutSeconds,omitempty"`
	Resources      ResourceRequirements `json:"resources,omitempty"`
	NodeSelector   map[string]string    `json:"nodeSelector,omitempty"`
	Tolerations    []Toleration         `json:"tolerations,omitempty"`
	Payload        string               `json:"payload,omitempty"`
}

type JobStatus struct {
	Phase     JobPhase    `json:"phase,omitempty"`
	Stage     JobStage    `json:"stage,omitempty"`
	Result    *JobResult  `json:"result,omitempty"`
	Runner    string      `json:"runner,omitempty"`
	StartTime metav1.Time `json:"startTime,omitempty"`
	EndTime   metav1.Time `json:"endTime,omitempty"`
	Message   string      `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Job `json:"items"`
}

// ScriptRef records the Script observed when a Job is created. It does not
// pin the content version used by a later execution attempt.
type ScriptRef struct {
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}

type Toleration struct {
	Key      string             `json:"key,omitempty"`
	Operator TolerationOperator `json:"operator,omitempty"`
	Value    string             `json:"value,omitempty"`
	Effect   TaintEffect        `json:"effect,omitempty"`
}

// JobResult records the script verdict independently of upload and the Job lifecycle.
// MissingDeps describes install-check failures and is only valid for InstallFailed.
type JobResult struct {
	Status      JobResultStatus       `json:"status"`
	MissingDeps map[string]MissingDep `json:"missingDeps,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Runner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RunnerSpec   `json:"spec,omitempty"`
	Status            RunnerStatus `json:"status,omitempty"`
}

type RunnerSpec struct {
	InstanceID    string        `json:"instanceID,omitempty"`
	Type          ExecutionType `json:"type,omitempty"`
	Arch          string        `json:"arch,omitempty"`
	Unschedulable bool          `json:"unschedulable,omitempty"`
	Taints        []RunnerTaint `json:"taints,omitempty"`
}

type RunnerStatus struct {
	Phase       RunnerPhase        `json:"phase,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
	Capacity    map[string]string  `json:"capacity,omitempty"`
	Allocatable map[string]string  `json:"allocatable,omitempty"`
	Addresses   []RunnerAddress    `json:"addresses,omitempty"`
	Info        RunnerInfo         `json:"info,omitempty"`
	Heartbeat   metav1.Time        `json:"heartbeat,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type RunnerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Runner `json:"items"`
}

type RunnerTaint struct {
	Key    string      `json:"key"`
	Value  string      `json:"value,omitempty"`
	Effect TaintEffect `json:"effect"`
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

type BuildTarget struct {
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
}

type PackageRepo struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
	Ref  string `json:"ref,omitempty"`
	// CommitID optionally pins the repository to a full SHA-1 commit.
	CommitID string `json:"commitID,omitempty"`
}

type BootstrapRepo struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type MissingDep struct {
	NeededBy        string            `json:"neededBy,omitempty"`
	VersionRequests VersionConstraint `json:"versionRequests,omitempty"`
}

type VersionConstraint struct {
	GT string `json:"gt,omitempty"`
	GE string `json:"ge,omitempty"`
	EQ string `json:"eq,omitempty"`
	LE string `json:"le,omitempty"`
	LT string `json:"lt,omitempty"`
}
