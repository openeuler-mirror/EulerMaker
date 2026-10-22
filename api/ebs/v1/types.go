package v1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:deepcopy-gen=true
// Config is a cluster-scoped, versioned text configuration. Consumers interpret
// Content according to the object's name; the API does not parse it.
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
	ConfigVisibilityPublic    ConfigVisibility = "Public"
	ConfigVisibilityOpsOnly   ConfigVisibility = "OpsOnly"
	BuildTargetConfigName                      = "build-target"
	BuildResourceConfigName                    = "build-resource"
	ServiceConfigName                          = "service-config"
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

// ServiceConfigContent defines the service endpoints in Config/service-config.
// +k8s:deepcopy-gen=true
type ServiceConfigContent struct {
	ArtifactManagers map[string]ArtifactManagerInstance `json:"artifactManagers" yaml:"artifactManagers"`
	GitServer        GitServerConfig                    `json:"gitServer" yaml:"gitServer"`
}

// +k8s:deepcopy-gen=true
type GitServerConfig struct {
	InternalURL string `json:"internalURL" yaml:"internalURL"`
}

// +k8s:deepcopy-gen=true
type ArtifactManagerInstance struct {
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

const ProjectArtifactsAnnotation = "ebs.io/artifacts"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProjectSpec   `json:"spec,omitempty"`
	Status            ProjectStatus `json:"status,omitempty"`
}

type ProjectSpec struct {
	DisplayName   string          `json:"displayName,omitempty"`
	Description   string          `json:"description,omitempty"`
	DefaultRef    string          `json:"defaultRef,omitempty"`
	BuildPayload  string          `json:"buildPayload,omitempty"`
	BuildTargets  []BuildTarget   `json:"buildTargets,omitempty"`
	PackageRepos  []PackageRepo   `json:"packageRepos,omitempty"`
	BootstrapRepo []BootstrapRepo `json:"bootstrapRepo,omitempty"`
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
	CloneURL string           `json:"cloneUrl,omitempty"`
	CommitID string           `json:"commitId,omitempty"`
	Error    *SpecCommitError `json:"error,omitempty"`
}

type SpecCommitError struct {
	Code      SpecCommitErrorCode `json:"code,omitempty"`
	Message   string              `json:"message,omitempty"`
	Retryable bool                `json:"retryable,omitempty"`
}

// BuildGitRefAnnotation overrides the source ref for a single-package Build.
const BuildGitRefAnnotation = "ebs.io/git-ref"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BuildSpec   `json:"spec,omitempty"`
	Status            BuildStatus `json:"status,omitempty"`
}

type BuildSpec struct {
	BuildType   string      `json:"buildType,omitempty"`
	Packages    []string    `json:"packages,omitempty"`
	BuildTarget BuildTarget `json:"buildTarget,omitempty"`
}

type BuildStatus struct {
	Phase        BuildPhase         `json:"phase,omitempty"`
	Stage        string             `json:"stage,omitempty"`
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
	BuildPayload  string          `json:"buildPayload,omitempty"`
	BootstrapRepo []BootstrapRepo `json:"bootstrapRepo,omitempty"`
}

// BuildInfoStatus records build results and unconfirmed Job creations.
type BuildInfoStatus struct {
	Phase             BuildInfoPhase              `json:"phase,omitempty"`
	Conditions        []metav1.Condition          `json:"conditions,omitempty"`
	SpecStatus        SpecStatusGroup             `json:"specStatus"`
	SpecRepoNames     map[string]string           `json:"specRepoNames,omitempty"`
	FailedPackages    []string                    `json:"failedPackages,omitempty"`
	PendingJobCreates map[string]PendingJobCreate `json:"pendingJobCreates,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BuildInfoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BuildInfo `json:"items"`
}

// PendingJobCreate records a Job creation identity that has been registered
// in status but whose creation result is not yet confirmed. Keyed by spec
// name; a non-empty map blocks writing Completed.
type PendingJobCreate struct {
	JobName            string `json:"jobName,omitempty"`
	DispatchGeneration int64  `json:"dispatchGeneration,omitempty"`
}

// SpecStatusGroup stores per-spec fields in separate maps so callers can
// project build and install results independently.
type SpecStatusGroup struct {
	Build         map[string]SpecBuildStatus   `json:"build,omitempty"`
	Install       map[string]SpecInstallStatus `json:"install,omitempty"`
	DispatchCount map[string]int64             `json:"dispatchCount,omitempty"`
}

// SpecStatus is the in-memory per-spec view of the grouped status fields.
type SpecStatus struct {
	Build         SpecBuildStatus   `json:"build,omitempty"`
	Install       SpecInstallStatus `json:"install,omitempty"`
	DispatchCount int64             `json:"dispatchCount,omitempty"`
}

type SpecBuildStatus struct {
	Status     string             `json:"status"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type SpecInstallStatus struct {
	Status      string                `json:"status"`
	MissingDeps map[string]MissingDep `json:"missingDeps,omitempty"`
	Conditions  []metav1.Condition    `json:"conditions,omitempty"`
}

// UnmarshalJSON accepts the former per-spec layout when reading stored
// BuildInfo objects. Subsequent writes use the grouped layout.
func (s *SpecStatusGroup) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	legacy := false
	for name := range fields {
		if name != "build" && name != "install" && name != "dispatchCount" {
			legacy = true
			break
		}
	}
	if legacy {
		var entries map[string]SpecStatus
		if err := json.Unmarshal(data, &entries); err != nil {
			return err
		}
		*s = NewSpecStatusGroup(entries)
		return nil
	}
	type grouped SpecStatusGroup
	var value grouped
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = SpecStatusGroup(value)
	return nil
}

// NewSpecStatusGroup converts per-spec statuses to the grouped API shape.
func NewSpecStatusGroup(entries map[string]SpecStatus) SpecStatusGroup {
	var group SpecStatusGroup
	for name, entry := range entries {
		group.Set(name, entry)
	}
	return group
}

func (s SpecStatusGroup) Entry(name string) SpecStatus {
	return SpecStatus{Build: s.Build[name], Install: s.Install[name], DispatchCount: s.DispatchCount[name]}
}

func (s SpecStatusGroup) Lookup(name string) (SpecStatus, bool) {
	_, build := s.Build[name]
	_, install := s.Install[name]
	_, count := s.DispatchCount[name]
	return s.Entry(name), build || install || count
}

func (s *SpecStatusGroup) Set(name string, status SpecStatus) {
	if s.Build == nil {
		s.Build = make(map[string]SpecBuildStatus)
	}
	s.Build[name] = status.Build
	if status.Install.Status != "" || len(status.Install.MissingDeps) > 0 || len(status.Install.Conditions) > 0 {
		if s.Install == nil {
			s.Install = make(map[string]SpecInstallStatus)
		}
		s.Install[name] = status.Install
	} else {
		delete(s.Install, name)
	}
	if status.DispatchCount != 0 {
		if s.DispatchCount == nil {
			s.DispatchCount = make(map[string]int64)
		}
		s.DispatchCount[name] = status.DispatchCount
	} else {
		delete(s.DispatchCount, name)
	}
}

func (s SpecStatusGroup) Len() int { return len(s.Build) }

func (s SpecStatusGroup) Entries() map[string]SpecStatus {
	entries := make(map[string]SpecStatus, len(s.Build))
	for name := range s.Build {
		entries[name] = s.Entry(name)
	}
	return entries
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// BuildGraph persists the dependency graph owned by the same-name BuildInfo.
type BuildGraph struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Status            BuildGraphStatus `json:"status"`
}

type BuildGraphStatus struct {
	Nodes map[string]DcgNodeState `json:"nodes"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BuildGraphList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BuildGraph `json:"items"`
}

// DcgNodeState is the persisted mirror of the in-memory DcgNode. OutDep
// lists downstream specs that depend on this spec; InDep/InstallInDep map
// upstream specs to the matched version constraints. BootstrapBreak marks
// a cycle-break node; it is persisted and never re-selected on load.
type DcgNodeState struct {
	Version        string                  `json:"version,omitempty"`
	OutDep         []string                `json:"outDep,omitempty"`
	InDep          map[string]VersionConst `json:"inDep,omitempty"`
	InstallInDep   map[string]VersionConst `json:"installInDep,omitempty"`
	BootstrapBreak bool                    `json:"bootstrapBreak,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type RpmRepo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RpmRepoSpec   `json:"spec,omitempty"`
	Status            RpmRepoStatus `json:"status,omitempty"`
}

type RpmRepoSpec struct{}

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
	Runtime        string               `json:"runtime,omitempty"`
	RuntimeSpec    runtime.RawExtension `json:"runtimeSpec,omitempty"`
	ScriptRefs     []ScriptRef          `json:"scriptRefs,omitempty"`
	TimeoutSeconds int64                `json:"timeoutSeconds,omitempty"`
	Resources      ResourceRequirements `json:"resources,omitempty"`
	NodeSelector   map[string]string    `json:"nodeSelector,omitempty"`
	Tolerations    []Toleration         `json:"tolerations,omitempty"`
	Payload        string               `json:"payload,omitempty"`
}

type JobStatus struct {
	Phase        JobPhase          `json:"phase,omitempty"`
	Stage        JobStage          `json:"stage,omitempty"`
	Build        *JobBuildResult   `json:"build,omitempty"`
	Install      *JobInstallResult `json:"install,omitempty"`
	Runner       string            `json:"runner,omitempty"`
	StartTime    metav1.Time       `json:"startTime,omitempty"`
	EndTime      metav1.Time       `json:"endTime,omitempty"`
	Message      string            `json:"message,omitempty"`
	RestartCount int64             `json:"restartCount,omitempty"`
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
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// JobBuildResult records the outcome of the build script, independently of
// artifact upload and the Job's overall lifecycle.
type JobBuildResult struct {
	Status JobResultStatus `json:"status"`
}

// JobInstallResult records the post-build installability check. MissingDeps
// is populated only for a deterministic dependency failure.
type JobInstallResult struct {
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
	InstanceID    string        `json:"instanceId,omitempty"`
	Type          string        `json:"type,omitempty"`
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
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
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
	Os          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	BuildFlag   bool   `json:"buildFlag,omitempty"`
	PublishFlag bool   `json:"publishFlag,omitempty"`
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
	Repo string `json:"repo,omitempty"`
}

type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type MissingDep struct {
	NeededBy        string       `json:"neededBy,omitempty"`
	VersionRequests VersionConst `json:"versionRequests,omitempty"`
}

type VersionConst struct {
	GT string `json:"gt,omitempty"`
	GE string `json:"ge,omitempty"`
	EQ string `json:"eq,omitempty"`
	LE string `json:"le,omitempty"`
	LT string `json:"lt,omitempty"`
}
