package v1

import "testing"

func TestProjectPhase(t *testing.T) {
	for _, phase := range []ProjectPhase{ProjectActive, ProjectTerminating} {
		if !phase.IsValid() {
			t.Errorf("phase %q is not valid", phase)
		}
	}
	if ProjectPhase("Unknown").IsValid() {
		t.Error("unknown phase is valid")
	}

	values := ProjectPhaseValues()
	values[0] = "changed"
	if ProjectPhaseValues()[0] != string(ProjectActive) {
		t.Error("ProjectPhaseValues exposes mutable internal state")
	}
}

func TestBuildInfoPhase(t *testing.T) {
	for _, phase := range []BuildInfoPhase{BuildInfoPending, BuildInfoProcessing, BuildInfoCompleted} {
		if !phase.IsValid() {
			t.Errorf("phase %q is not valid", phase)
		}
	}
	if BuildInfoPhase("Unknown").IsValid() {
		t.Error("unknown phase is valid")
	}

	values := BuildInfoPhaseValues()
	values[0] = "changed"
	if BuildInfoPhaseValues()[0] != string(BuildInfoPending) {
		t.Error("BuildInfoPhaseValues exposes mutable internal state")
	}
}

func TestJobPhase(t *testing.T) {
	for _, phase := range []JobPhase{JobPending, JobRunning, JobCompleted, JobFailed, JobAborted} {
		if !phase.IsValid() {
			t.Errorf("phase %q is not valid", phase)
		}
	}
	if JobPhase("Unknown").IsValid() {
		t.Error("unknown phase is valid")
	}
	for _, phase := range []JobPhase{JobCompleted, JobFailed, JobAborted} {
		if !phase.IsTerminal() {
			t.Errorf("phase %q is not terminal", phase)
		}
	}
	if JobRunning.IsTerminal() {
		t.Error("running phase is terminal")
	}
	values := JobPhaseValues()
	values[0] = "changed"
	if JobPhaseValues()[0] != string(JobPending) {
		t.Error("JobPhaseValues exposes mutable internal state")
	}
}

func TestJobStage(t *testing.T) {
	for _, stage := range []JobStage{JobStagePending, JobStageRunning, JobStagePostRun} {
		if !stage.IsValid() {
			t.Errorf("stage %q is not valid", stage)
		}
	}
	if JobStage("Unknown").IsValid() {
		t.Error("unknown stage is valid")
	}
	values := JobStageValues()
	values[0] = "changed"
	if JobStageValues()[0] != string(JobStagePending) {
		t.Error("JobStageValues exposes mutable internal state")
	}
}

func TestRunnerPhase(t *testing.T) {
	for _, phase := range []RunnerPhase{RunnerOnline, RunnerOffline} {
		if !phase.IsValid() {
			t.Errorf("phase %q is not valid", phase)
		}
	}
	if RunnerPhase("Unknown").IsValid() {
		t.Error("unknown phase is valid")
	}

	values := RunnerPhaseValues()
	values[0] = "changed"
	if RunnerPhaseValues()[0] != string(RunnerOnline) {
		t.Error("RunnerPhaseValues exposes mutable internal state")
	}
}

func TestSnapshotPhase(t *testing.T) {
	for _, phase := range []SnapshotPhase{SnapshotPending, SnapshotProcessing, SnapshotActive} {
		if !phase.IsValid() {
			t.Errorf("phase %q is not valid", phase)
		}
	}
	if SnapshotPhase("Unknown").IsValid() {
		t.Error("unknown phase is valid")
	}

	values := SnapshotPhaseValues()
	values[0] = "changed"
	if SnapshotPhaseValues()[0] != string(SnapshotPending) {
		t.Error("SnapshotPhaseValues exposes mutable internal state")
	}
}

func TestBuildPhase(t *testing.T) {
	for _, phase := range []BuildPhase{
		BuildPending, BuildPrepared, BuildProcessing,
		BuildSuccess, BuildFailed, BuildAborted, BuildSkipped,
	} {
		if !phase.IsValid() {
			t.Errorf("phase %q is not valid", phase)
		}
	}
	if BuildPhase("Unknown").IsValid() {
		t.Error("unknown phase is valid")
	}
	for _, phase := range []BuildPhase{BuildSuccess, BuildFailed, BuildAborted, BuildSkipped} {
		if !phase.IsTerminal() {
			t.Errorf("phase %q is not terminal", phase)
		}
	}
	if BuildProcessing.IsTerminal() {
		t.Error("processing phase is terminal")
	}

	values := BuildPhaseValues()
	values[0] = "changed"
	if BuildPhaseValues()[0] != string(BuildPending) {
		t.Error("BuildPhaseValues exposes mutable internal state")
	}
}

func TestRpmRepoReleasePhase(t *testing.T) {
	for _, phase := range []RpmRepoReleasePhase{
		RpmRepoReleasePending, RpmRepoReleaseCreating, RpmRepoReleasePrepared,
		RpmRepoReleaseReady, RpmRepoReleaseFailed,
	} {
		if !phase.IsValid() {
			t.Errorf("phase %q is not valid", phase)
		}
	}
	if RpmRepoReleasePhase("Unknown").IsValid() {
		t.Error("unknown release phase is valid")
	}
	values := RpmRepoReleasePhaseValues()
	values[0] = "changed"
	if RpmRepoReleasePhaseValues()[0] != string(RpmRepoReleasePending) {
		t.Error("RpmRepoReleasePhaseValues exposes mutable internal state")
	}
}
