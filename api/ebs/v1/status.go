package v1

// ExecutionType identifies the Job execution environment, not the Docker/containerd implementation.
type ExecutionType string

const (
	ExecutionTypeContainer ExecutionType = "ct"
	ExecutionTypeVM        ExecutionType = "vm"
	ExecutionTypeHardware  ExecutionType = "hw"
)

func (t ExecutionType) IsValid() bool {
	return t == ExecutionTypeContainer || t == ExecutionTypeVM || t == ExecutionTypeHardware
}

func ExecutionTypeValues() []string {
	return []string{string(ExecutionTypeContainer), string(ExecutionTypeVM), string(ExecutionTypeHardware)}
}

// TolerationOperator selects how a toleration matches a taint. An omitted operator means Equal.
type TolerationOperator string

const (
	TolerationEqual  TolerationOperator = "Equal"
	TolerationExists TolerationOperator = "Exists"
)

func (o TolerationOperator) IsValid() bool {
	return o == TolerationEqual || o == TolerationExists
}

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

func (e TaintEffect) IsValid() bool {
	return e == TaintEffectNoSchedule || e == TaintEffectPreferNoSchedule || e == TaintEffectNoExecute
}

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

func BuildTypeValues() []string {
	return []string{string(BuildTypeFull), string(BuildTypeIncremental), string(BuildTypeSpecified), string(BuildTypeSingle)}
}

func (v BuildType) IsValid() bool {
	switch v {
	case BuildTypeFull, BuildTypeIncremental, BuildTypeSpecified, BuildTypeSingle:
		return true
	default:
		return false
	}
}

type BuildStage string

const (
	BuildStageBuild   BuildStage = "build"
	BuildStagePublish BuildStage = "publish"
)

func BuildStageValues() []string {
	return []string{string(BuildStageBuild), string(BuildStagePublish)}
}

func (v BuildStage) IsValid() bool {
	switch v {
	case BuildStageBuild, BuildStagePublish:
		return true
	default:
		return false
	}
}

// SpecResultPhase describes a repository/spec result throughout preparation, build and install checking.
type SpecResultPhase string

const (
	SpecPending           SpecResultPhase = "Pending"
	SpecPrepared          SpecResultPhase = "Prepared"
	SpecPrepareFailed     SpecResultPhase = "PrepareFailed"
	SpecRunning           SpecResultPhase = "Running"
	SpecBuildFailed       SpecResultPhase = "BuildFailed"
	SpecRpmDependsMissing SpecResultPhase = "RpmDependsMissing"
	SpecInstallFailed     SpecResultPhase = "InstallFailed"
	SpecSucceeded         SpecResultPhase = "Succeeded"
	SpecArchUnsupported   SpecResultPhase = "ArchUnsupported"
)

func SpecResultPhaseValues() []string {
	return []string{
		string(SpecPending),
		string(SpecPrepared),
		string(SpecPrepareFailed),
		string(SpecRunning),
		string(SpecBuildFailed),
		string(SpecRpmDependsMissing),
		string(SpecInstallFailed),
		string(SpecSucceeded),
		string(SpecArchUnsupported),
	}
}

func (s SpecResultPhase) IsValid() bool {
	switch s {
	case SpecPending, SpecPrepared, SpecPrepareFailed, SpecRunning, SpecBuildFailed, SpecRpmDependsMissing,
		SpecInstallFailed, SpecSucceeded, SpecArchUnsupported:
		return true
	default:
		return false
	}
}

// BuildSucceeded reports whether usable build output exists, regardless of installation dependencies.
func (s SpecResultPhase) BuildSucceeded() bool {
	return s == SpecSucceeded || s == SpecInstallFailed
}

func (s SpecResultPhase) IsTerminal() bool {
	return s.BuildSucceeded() || s == SpecPrepareFailed || s == SpecBuildFailed ||
		s == SpecRpmDependsMissing || s == SpecArchUnsupported
}

type ProjectPhase string

const (
	ProjectActive      ProjectPhase = "Active"
	ProjectTerminating ProjectPhase = "Terminating"
)

var projectPhaseValues = []string{
	string(ProjectActive),
	string(ProjectTerminating),
}

func ProjectPhaseValues() []string {
	return append([]string(nil), projectPhaseValues...)
}

func (p ProjectPhase) IsValid() bool {
	return p == ProjectActive || p == ProjectTerminating
}

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

func BuildInfoPhaseValues() []string {
	return append([]string(nil), buildInfoPhaseValues...)
}

func (p BuildInfoPhase) IsValid() bool {
	switch p {
	case BuildInfoPending, BuildInfoProcessing, BuildInfoCompleted, BuildInfoFailed, BuildInfoAborted:
		return true
	default:
		return false
	}
}

func (p BuildInfoPhase) IsTerminal() bool {
	return p == BuildInfoCompleted || p == BuildInfoFailed || p == BuildInfoAborted
}

type JobPhase string

// JobResultStatus describes the script verdict. An absent result means no reliable verdict is available.
type JobResultStatus string

const (
	JobResultSucceeded     JobResultStatus = "Succeeded"
	JobResultBuildFailed   JobResultStatus = "BuildFailed"
	JobResultInstallFailed JobResultStatus = "InstallFailed"
)

func (s JobResultStatus) IsValid() bool {
	return s == JobResultSucceeded || s == JobResultBuildFailed || s == JobResultInstallFailed
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

func JobPhaseValues() []string {
	return append([]string(nil), jobPhaseValues...)
}

func (p JobPhase) IsValid() bool {
	switch p {
	case JobPending, JobRunning, JobSucceeded, JobFailed, JobAborted:
		return true
	default:
		return false
	}
}

func (p JobPhase) IsTerminal() bool {
	return p == JobSucceeded || p == JobFailed || p == JobAborted
}

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

func JobStageValues() []string {
	return append([]string(nil), jobStageValues...)
}

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

func RunnerPhaseValues() []string {
	return append([]string(nil), runnerPhaseValues...)
}

func (p RunnerPhase) IsValid() bool {
	return p == RunnerOnline || p == RunnerOffline || p == RunnerEvicted
}

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

func SnapshotPhaseValues() []string {
	return append([]string(nil), snapshotPhaseValues...)
}

func (p SnapshotPhase) IsValid() bool {
	switch p {
	case SnapshotPending, SnapshotProcessing, SnapshotActive:
		return true
	default:
		return false
	}
}

type BuildPhase string

const (
	BuildPending    BuildPhase = "Pending"
	BuildPrepared   BuildPhase = "Prepared"
	BuildProcessing BuildPhase = "Processing"
	BuildSuccess    BuildPhase = "Success"
	BuildFailed     BuildPhase = "Failed"
	BuildAborted    BuildPhase = "Aborted"
)

var buildPhaseValues = []string{
	string(BuildPending),
	string(BuildPrepared),
	string(BuildProcessing),
	string(BuildSuccess),
	string(BuildFailed),
	string(BuildAborted),
}

func BuildPhaseValues() []string {
	return append([]string(nil), buildPhaseValues...)
}

func (p BuildPhase) IsValid() bool {
	switch p {
	case BuildPending, BuildPrepared, BuildProcessing, BuildSuccess, BuildFailed, BuildAborted:
		return true
	default:
		return false
	}
}

func (p BuildPhase) IsTerminal() bool {
	switch p {
	case BuildSuccess, BuildFailed, BuildAborted:
		return true
	default:
		return false
	}
}

// RpmRepo condition types distinguish repository batches from formal releases.
const (
	RpmRepoConditionRepositoryReady = "RepositoryReady"
	RpmRepoConditionPublishSucceed  = "PublishSucceed"
	RpmRepoConditionCleanupReady    = "CleanupReady"
)

// RpmRepo condition reasons describe repository, release and cleanup outcomes.
const (
	RpmRepoReasonRepositoryCreated        = "RepositoryCreated"
	RpmRepoReasonRepositoryCreationFailed = "RepositoryCreationFailed"
	RpmRepoReasonReleaseActivated         = "ReleaseActivated"
	RpmRepoReasonReleaseFailed            = "ReleaseFailed"
	RpmRepoReasonNoPublishableArtifacts   = "NoPublishableArtifacts"
	RpmRepoReasonBuildAborted             = "BuildAborted"
	RpmRepoReasonBuildInfoFailed          = "BuildInfoFailed"
	RpmRepoReasonOperationsDrained        = "OperationsDrained"
)

// RpmRepoReleasePhase describes formal release preparation and activation.
type RpmRepoReleasePhase string

const (
	RpmRepoReleasePending  RpmRepoReleasePhase = "Pending"
	RpmRepoReleaseCreating RpmRepoReleasePhase = "Creating"
	RpmRepoReleaseReady    RpmRepoReleasePhase = "Ready"
	RpmRepoReleaseFailed   RpmRepoReleasePhase = "Failed"
	RpmRepoReleaseSkipped  RpmRepoReleasePhase = "Skipped"
)

var rpmRepoReleasePhaseValues = []string{
	string(RpmRepoReleasePending),
	string(RpmRepoReleaseCreating),
	string(RpmRepoReleaseReady),
	string(RpmRepoReleaseFailed),
	string(RpmRepoReleaseSkipped),
}

func RpmRepoReleasePhaseValues() []string {
	return append([]string(nil), rpmRepoReleasePhaseValues...)
}

func (p RpmRepoReleasePhase) IsValid() bool {
	switch p {
	case RpmRepoReleasePending, RpmRepoReleaseCreating,
		RpmRepoReleaseReady, RpmRepoReleaseFailed, RpmRepoReleaseSkipped:
		return true
	default:
		return false
	}
}
