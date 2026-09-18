package v1

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
)

var buildInfoPhaseValues = []string{
	string(BuildInfoPending),
	string(BuildInfoProcessing),
	string(BuildInfoCompleted),
}

// BuildInfoPhaseValues returns all valid BuildInfo phase values.
func BuildInfoPhaseValues() []string {
	return append([]string(nil), buildInfoPhaseValues...)
}

// IsValid reports whether p is a supported BuildInfo phase.
func (p BuildInfoPhase) IsValid() bool {
	return p == BuildInfoPending || p == BuildInfoProcessing || p == BuildInfoCompleted
}

// JobPhase describes the lifecycle state of a Job.
type JobPhase string

const (
	JobPending   JobPhase = "Pending"
	JobRunning   JobPhase = "Running"
	JobCompleted JobPhase = "Completed"
	JobFailed    JobPhase = "Failed"
	JobAborted   JobPhase = "Aborted"
)

var jobPhaseValues = []string{
	string(JobPending),
	string(JobRunning),
	string(JobCompleted),
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
	case JobPending, JobRunning, JobCompleted, JobFailed, JobAborted:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether no more Job execution is expected for p.
func (p JobPhase) IsTerminal() bool {
	return p == JobCompleted || p == JobFailed || p == JobAborted
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
)

var runnerPhaseValues = []string{
	string(RunnerOnline),
	string(RunnerOffline),
}

// RunnerPhaseValues returns all valid Runner phase values.
func RunnerPhaseValues() []string {
	return append([]string(nil), runnerPhaseValues...)
}

// IsValid reports whether p is a supported Runner phase.
func (p RunnerPhase) IsValid() bool {
	return p == RunnerOnline || p == RunnerOffline
}

// SnapshotPhase describes the lifecycle state of a Snapshot.
type SnapshotPhase string

// SpecCommitErrorCode classifies a package repository resolution failure.
type SpecCommitErrorCode string

const (
	SpecCommitValidationFailed SpecCommitErrorCode = "ValidationFailed"
	SpecCommitSyncFailed       SpecCommitErrorCode = "SyncFailed"
	SpecCommitSyncTimeout      SpecCommitErrorCode = "SyncTimeout"
	SpecCommitResolveFailed    SpecCommitErrorCode = "ResolveFailed"
	SpecCommitCommitConflict   SpecCommitErrorCode = "CommitConflict"
	SpecCommitRetryExhausted   SpecCommitErrorCode = "RetryExhausted"
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

// RpmRepoPhase describes the lifecycle of the current process repository version.
type RpmRepoPhase string

const (
	RpmRepoProcessing RpmRepoPhase = "Processing"
	RpmRepoReady      RpmRepoPhase = "Ready"
	RpmRepoFailed     RpmRepoPhase = "Failed"
)

var rpmRepoPhaseValues = []string{
	string(RpmRepoProcessing),
	string(RpmRepoReady),
	string(RpmRepoFailed),
}

// RpmRepoPhaseValues returns all valid RpmRepo phase values.
func RpmRepoPhaseValues() []string {
	return append([]string(nil), rpmRepoPhaseValues...)
}

// IsValid reports whether p is a supported RpmRepo phase.
func (p RpmRepoPhase) IsValid() bool {
	switch p {
	case RpmRepoProcessing, RpmRepoReady, RpmRepoFailed:
		return true
	default:
		return false
	}
}

// RpmRepoReleasePhase describes formal release preparation and activation.
type RpmRepoReleasePhase string

const (
	RpmRepoReleasePending  RpmRepoReleasePhase = "Pending"
	RpmRepoReleaseCreating RpmRepoReleasePhase = "Creating"
	RpmRepoReleasePrepared RpmRepoReleasePhase = "Prepared"
	RpmRepoReleaseReady    RpmRepoReleasePhase = "Ready"
	RpmRepoReleaseFailed   RpmRepoReleasePhase = "Failed"
)

var rpmRepoReleasePhaseValues = []string{
	string(RpmRepoReleasePending),
	string(RpmRepoReleaseCreating),
	string(RpmRepoReleasePrepared),
	string(RpmRepoReleaseReady),
	string(RpmRepoReleaseFailed),
}

// RpmRepoReleasePhaseValues returns all valid formal release phase values.
func RpmRepoReleasePhaseValues() []string {
	return append([]string(nil), rpmRepoReleasePhaseValues...)
}

// IsValid reports whether p is a supported formal release phase.
func (p RpmRepoReleasePhase) IsValid() bool {
	switch p {
	case RpmRepoReleasePending, RpmRepoReleaseCreating, RpmRepoReleasePrepared,
		RpmRepoReleaseReady, RpmRepoReleaseFailed:
		return true
	default:
		return false
	}
}
