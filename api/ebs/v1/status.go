package v1

// ExecutionType identifies the Job execution environment, not the Docker/containerd implementation.
type ExecutionType string

const (
	ExecutionTypeContainer ExecutionType = "ct"
	ExecutionTypeVM        ExecutionType = "vm"
	ExecutionTypeHardware  ExecutionType = "hw"
)

// IsValid reports whether the execution type is supported by the API.
func (t ExecutionType) IsValid() bool {
	return t == ExecutionTypeContainer || t == ExecutionTypeVM || t == ExecutionTypeHardware
}

// ExecutionTypeValues returns all supported execution types.
func ExecutionTypeValues() []string {
	return []string{string(ExecutionTypeContainer), string(ExecutionTypeVM), string(ExecutionTypeHardware)}
}

// TolerationOperator selects how a toleration matches a taint. An omitted operator means Equal.
type TolerationOperator string

const (
	TolerationEqual  TolerationOperator = "Equal"
	TolerationExists TolerationOperator = "Exists"
)

// IsValid reports whether the operator is supported.
func (o TolerationOperator) IsValid() bool {
	return o == TolerationEqual || o == TolerationExists
}

// TolerationOperatorValues returns the supported nonempty operators.
func TolerationOperatorValues() []string {
	return []string{string(TolerationEqual), string(TolerationExists)}
}

// TaintEffect describes the scheduling effect of a Runner taint.
type TaintEffect string

const (
	TaintEffectNoSchedule       TaintEffect = "NoSchedule"
	TaintEffectPreferNoSchedule TaintEffect = "PreferNoSchedule"
	TaintEffectNoExecute        TaintEffect = "NoExecute"
)

// IsValid reports whether the effect is supported.
func (e TaintEffect) IsValid() bool {
	return e == TaintEffectNoSchedule || e == TaintEffectPreferNoSchedule || e == TaintEffectNoExecute
}

// TaintEffectValues returns the supported nonempty effects.
func TaintEffectValues() []string {
	return []string{string(TaintEffectNoSchedule), string(TaintEffectPreferNoSchedule), string(TaintEffectNoExecute)}
}

// BuildType selects the package selection strategy.
type BuildType string

const (
	BuildTypeFull        BuildType = "full"
	BuildTypeIncremental BuildType = "incremental"
	BuildTypeSpecified   BuildType = "specified"
	BuildTypeSingle      BuildType = "single"
)

// BuildTypeValues returns the supported nonempty values.
func BuildTypeValues() []string {
	return []string{string(BuildTypeFull), string(BuildTypeIncremental), string(BuildTypeSpecified), string(BuildTypeSingle)}
}

// IsValid reports whether the value is supported.
func (v BuildType) IsValid() bool {
	switch v {
	case BuildTypeFull, BuildTypeIncremental, BuildTypeSpecified, BuildTypeSingle:
		return true
	default:
		return false
	}
}

// BuildStage describes the current build or publication stage.
type BuildStage string

const (
	BuildStageBuild   BuildStage = "build"
	BuildStagePublish BuildStage = "publish"
)

// BuildStageValues returns the supported nonempty values.
func BuildStageValues() []string {
	return []string{string(BuildStageBuild), string(BuildStagePublish)}
}

// IsValid reports whether the value is supported.
func (v BuildStage) IsValid() bool {
	switch v {
	case BuildStageBuild, BuildStagePublish:
		return true
	default:
		return false
	}
}

// SpecBuildPhase describes a spec build verdict; an empty value means no verdict yet.
type SpecBuildPhase string

const (
	SpecBuildRunning         SpecBuildPhase = "Running"
	SpecBuildSucceeded       SpecBuildPhase = "Succeeded"
	SpecBuildFailed          SpecBuildPhase = "Failed"
	SpecBuildArchUnsupported SpecBuildPhase = "ArchUnsupported"
)

// SpecBuildPhaseValues returns the supported nonempty values.
func SpecBuildPhaseValues() []string {
	return []string{string(SpecBuildRunning), string(SpecBuildSucceeded), string(SpecBuildFailed), string(SpecBuildArchUnsupported)}
}

// IsValid reports whether the value is supported.
func (v SpecBuildPhase) IsValid() bool {
	switch v {
	case SpecBuildRunning, SpecBuildSucceeded, SpecBuildFailed, SpecBuildArchUnsupported:
		return true
	default:
		return false
	}
}

// SpecInstallPhase describes an install-check verdict; an empty value means not assessed.
type SpecInstallPhase string

const (
	SpecInstallSucceeded SpecInstallPhase = "Succeeded"
	SpecInstallFailed    SpecInstallPhase = "Failed"
)

// SpecInstallPhaseValues returns the supported nonempty values.
func SpecInstallPhaseValues() []string {
	return []string{string(SpecInstallSucceeded), string(SpecInstallFailed)}
}

// IsValid reports whether the value is supported.
func (v SpecInstallPhase) IsValid() bool {
	switch v {
	case SpecInstallSucceeded, SpecInstallFailed:
		return true
	default:
		return false
	}
}

// ProjectPhase describes the lifecycle state of a Project.
type ProjectPhase string

const (
	ProjectActive      ProjectPhase = "Active"
	ProjectTerminating ProjectPhase = "Terminating"
)

var projectPhaseValues = []string{
	string(ProjectActive),
	string(ProjectTerminating),
}

// ProjectPhaseValues returns all valid Project phase values.
func ProjectPhaseValues() []string {
	return append([]string(nil), projectPhaseValues...)
}

// IsValid reports whether p is a supported Project phase.
func (p ProjectPhase) IsValid() bool {
	return p == ProjectActive || p == ProjectTerminating
}

// BuildInfoPhase describes the lifecycle state of BuildInfo generation.
type BuildInfoPhase string

const (
	BuildInfoPending    BuildInfoPhase = "Pending"
	BuildInfoProcessing BuildInfoPhase = "Processing"
	BuildInfoCompleted  BuildInfoPhase = "Completed"
	BuildInfoFailed     BuildInfoPhase = "Failed"
	BuildInfoAborted    BuildInfoPhase = "Aborted"
)

var buildInfoPhaseValues = []string{
	string(BuildInfoPending),
	string(BuildInfoProcessing),
	string(BuildInfoCompleted),
	string(BuildInfoFailed),
	string(BuildInfoAborted),
}

// BuildInfoPhaseValues returns all valid BuildInfo phase values.
func BuildInfoPhaseValues() []string {
	return append([]string(nil), buildInfoPhaseValues...)
}

// IsValid reports whether p is a supported BuildInfo phase.
func (p BuildInfoPhase) IsValid() bool {
	switch p {
	case BuildInfoPending, BuildInfoProcessing, BuildInfoCompleted, BuildInfoFailed, BuildInfoAborted:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether no more BuildInfo work is expected for p.
func (p BuildInfoPhase) IsTerminal() bool {
	return p == BuildInfoCompleted || p == BuildInfoFailed || p == BuildInfoAborted
}

// JobPhase describes the lifecycle state of a Job.
type JobPhase string

// JobResultStatus describes a completed build or install-check step. An
// absent result means the step has not produced a reliable verdict.
type JobResultStatus string

const (
	JobResultSucceeded JobResultStatus = "Succeeded"
	JobResultFailed    JobResultStatus = "Failed"
)

func (s JobResultStatus) IsValid() bool {
	return s == JobResultSucceeded || s == JobResultFailed
}

const (
	JobPending   JobPhase = "Pending"
	JobRunning   JobPhase = "Running"
	JobSucceeded JobPhase = "Succeeded"
	JobFailed    JobPhase = "Failed"
	JobAborted   JobPhase = "Aborted"
)

var jobPhaseValues = []string{
	string(JobPending),
	string(JobRunning),
	string(JobSucceeded),
	string(JobFailed),
	string(JobAborted),
}

// JobPhaseValues returns all valid Job phase values.
func JobPhaseValues() []string {
	return append([]string(nil), jobPhaseValues...)
}

// IsValid reports whether p is a supported Job phase.
func (p JobPhase) IsValid() bool {
	switch p {
	case JobPending, JobRunning, JobSucceeded, JobFailed, JobAborted:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether no more Job execution is expected for p.
func (p JobPhase) IsTerminal() bool {
	return p == JobSucceeded || p == JobFailed || p == JobAborted
}

// JobStage describes the current execution stage of a Job.
type JobStage string

const (
	JobStagePending JobStage = "Pending"
	JobStageRunning JobStage = "Running"
	JobStagePostRun JobStage = "PostRun"
)

var jobStageValues = []string{
	string(JobStagePending),
	string(JobStageRunning),
	string(JobStagePostRun),
}

// JobStageValues returns all valid Job stage values.
func JobStageValues() []string {
	return append([]string(nil), jobStageValues...)
}

// IsValid reports whether s is a supported Job stage.
func (s JobStage) IsValid() bool {
	return s == JobStagePending || s == JobStageRunning || s == JobStagePostRun
}

// RunnerPhase describes whether a Runner can accept Jobs.
type RunnerPhase string

const (
	RunnerOnline  RunnerPhase = "Online"
	RunnerOffline RunnerPhase = "Offline"
	RunnerEvicted RunnerPhase = "Evicted"
)

var runnerPhaseValues = []string{
	string(RunnerOnline),
	string(RunnerOffline),
	string(RunnerEvicted),
}

// RunnerPhaseValues returns all valid Runner phase values.
func RunnerPhaseValues() []string {
	return append([]string(nil), runnerPhaseValues...)
}

// IsValid reports whether p is a supported Runner phase.
func (p RunnerPhase) IsValid() bool {
	return p == RunnerOnline || p == RunnerOffline || p == RunnerEvicted
}

// SnapshotPhase describes the lifecycle state of a Snapshot.
type SnapshotPhase string

// PackageRepoErrorCode classifies a package repository resolution failure.
type PackageRepoErrorCode string

const (
	PackageRepoValidationFailed PackageRepoErrorCode = "ValidationFailed"
	PackageRepoSyncFailed       PackageRepoErrorCode = "SyncFailed"
	PackageRepoSyncTimeout      PackageRepoErrorCode = "SyncTimeout"
	PackageRepoResolveFailed    PackageRepoErrorCode = "ResolveFailed"
	PackageRepoCommitConflict   PackageRepoErrorCode = "CommitConflict"
	PackageRepoRetryExhausted   PackageRepoErrorCode = "RetryExhausted"
)

const (
	SnapshotPending    SnapshotPhase = "Pending"
	SnapshotProcessing SnapshotPhase = "Processing"
	SnapshotActive     SnapshotPhase = "Active"
)

var snapshotPhaseValues = []string{
	string(SnapshotPending),
	string(SnapshotProcessing),
	string(SnapshotActive),
}

// SnapshotPhaseValues returns all valid Snapshot phase values.
func SnapshotPhaseValues() []string {
	return append([]string(nil), snapshotPhaseValues...)
}

// IsValid reports whether p is a supported Snapshot phase.
func (p SnapshotPhase) IsValid() bool {
	switch p {
	case SnapshotPending, SnapshotProcessing, SnapshotActive:
		return true
	default:
		return false
	}
}

// BuildPhase describes the lifecycle state of a Build.
type BuildPhase string

const (
	BuildPending    BuildPhase = "Pending"
	BuildPrepared   BuildPhase = "Prepared"
	BuildProcessing BuildPhase = "Processing"
	BuildSuccess    BuildPhase = "Success"
	BuildFailed     BuildPhase = "Failed"
	BuildAborted    BuildPhase = "Aborted"
	BuildSkipped    BuildPhase = "Skipped"
)

var buildPhaseValues = []string{
	string(BuildPending),
	string(BuildPrepared),
	string(BuildProcessing),
	string(BuildSuccess),
	string(BuildFailed),
	string(BuildAborted),
	string(BuildSkipped),
}

// BuildPhaseValues returns all valid Build phase values.
func BuildPhaseValues() []string {
	return append([]string(nil), buildPhaseValues...)
}

// IsValid reports whether p is a supported Build phase.
func (p BuildPhase) IsValid() bool {
	switch p {
	case BuildPending, BuildPrepared, BuildProcessing,
		BuildSuccess, BuildFailed, BuildAborted, BuildSkipped:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether no more Build work is expected for p.
func (p BuildPhase) IsTerminal() bool {
	switch p {
	case BuildSuccess, BuildFailed, BuildAborted, BuildSkipped:
		return true
	default:
		return false
	}
}

// RpmRepo condition types distinguish repository batches from formal releases.
const (
	RpmRepoConditionRepositoryReady = "RepositoryReady"
	RpmRepoConditionPublishSucceed  = "PublishSucceed"
)

// RpmRepo condition reasons describe the outcome of repository or release work.
const (
	RpmRepoReasonRepositoryCreated        = "RepositoryCreated"
	RpmRepoReasonRepositoryCreationFailed = "RepositoryCreationFailed"
	RpmRepoReasonReleaseActivated         = "ReleaseActivated"
	RpmRepoReasonReleaseFailed            = "ReleaseFailed"
	RpmRepoReasonNoPublishableArtifacts   = "NoPublishableArtifacts"
	RpmRepoReasonBuildAborted             = "BuildAborted"
	RpmRepoReasonBuildInfoFailed          = "BuildInfoFailed"
)

// RpmRepoReleasePhase describes formal release preparation and activation.
type RpmRepoReleasePhase string

const (
	RpmRepoReleasePending  RpmRepoReleasePhase = "Pending"
	RpmRepoReleaseCreating RpmRepoReleasePhase = "Creating"
	RpmRepoReleasePrepared RpmRepoReleasePhase = "Prepared"
	RpmRepoReleaseReady    RpmRepoReleasePhase = "Ready"
	RpmRepoReleaseFailed   RpmRepoReleasePhase = "Failed"
	RpmRepoReleaseSkipped  RpmRepoReleasePhase = "Skipped"
)

var rpmRepoReleasePhaseValues = []string{
	string(RpmRepoReleasePending),
	string(RpmRepoReleaseCreating),
	string(RpmRepoReleasePrepared),
	string(RpmRepoReleaseReady),
	string(RpmRepoReleaseFailed),
	string(RpmRepoReleaseSkipped),
}

// RpmRepoReleasePhaseValues returns all valid formal release phase values.
func RpmRepoReleasePhaseValues() []string {
	return append([]string(nil), rpmRepoReleasePhaseValues...)
}

// IsValid reports whether p is a supported formal release phase.
func (p RpmRepoReleasePhase) IsValid() bool {
	switch p {
	case RpmRepoReleasePending, RpmRepoReleaseCreating, RpmRepoReleasePrepared,
		RpmRepoReleaseReady, RpmRepoReleaseFailed, RpmRepoReleaseSkipped:
		return true
	default:
		return false
	}
}
