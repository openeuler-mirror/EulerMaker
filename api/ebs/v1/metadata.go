package v1

const (
	ProjectTypeLabel     = "project.ebs.io/type"
	BuildTargetArchLabel = "ebs.io/target-arch"
	BuildTypeLabel       = "ebs.io/build-type"
	JobBuildNameLabel    = "ebs.io/build-name"
	JobSpecNameLabel     = "ebs.io/spec-name"
	JobPackageNameLabel  = "ebs.io/package-name"
)

const (
	ProjectTypeCommunity = "community"
	ProjectTypePersonal  = "personal"
)

const (
	// ProjectArtifactsAnnotation selects the Artifact Manager instance for a Project and its Jobs.
	ProjectArtifactsAnnotation = "ebs.io/artifacts"
	// BuildGitRefAnnotation overrides the source ref for a single-package Build.
	BuildGitRefAnnotation = "ebs.io/git-ref"
	// JobArchiveActionAnnotation is owned by the archive lifecycle API.
	JobArchiveActionAnnotation = "ebs.io/job-archive-action"
)

const (
	JobArchiveFinalizer          = "ebs.io/job-archive"
	JobArtifactsCleanupFinalizer = "ebs.io/job-artifacts-cleanup"
)
