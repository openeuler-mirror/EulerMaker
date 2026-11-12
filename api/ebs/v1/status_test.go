package v1

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func checkEnum[T interface {
	~string
	IsValid() bool
}](t *testing.T, values func() []string, expected string) {
	t.Helper()
	want := strings.Fields(expected)
	got := values()
	if !slices.Equal(got, want) {
		t.Fatalf("values = %v, want %v", got, want)
	}
	for _, value := range want {
		if !T(value).IsValid() {
			t.Errorf("value %q is not valid", value)
		}
	}
	if T("Unknown").IsValid() {
		t.Error("unknown value is valid")
	}
	got[0] = "changed"
	if !slices.Equal(values(), want) {
		t.Error("values exposes mutable internal state")
	}
}

func checkTerminalPhases[T interface {
	~string
	IsTerminal() bool
}](t *testing.T, values []string, terminal string) {
	t.Helper()
	terminalValues := strings.Fields(terminal)
	for _, value := range values {
		want := slices.Contains(terminalValues, value)
		if got := T(value).IsTerminal(); got != want {
			t.Errorf("%s terminal = %t, want %t", value, got, want)
		}
	}
}

func TestExecutionType(t *testing.T) {
	checkEnum[ExecutionType](t, ExecutionTypeValues, "ct vm hw")
	for _, value := range ExecutionTypeValues() {
		executionType := ExecutionType(value)
		data, err := json.Marshal(executionType)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != `"`+value+`"` {
			t.Fatalf("unexpected encoding: %s", data)
		}
		var decoded ExecutionType
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded != executionType {
			t.Fatalf("round trip = %q, want %q", decoded, executionType)
		}
	}
}

func TestTolerationOperator(t *testing.T) {
	checkEnum[TolerationOperator](t, TolerationOperatorValues, "Equal Exists")
}

func TestTaintEffect(t *testing.T) {
	checkEnum[TaintEffect](t, TaintEffectValues, "NoSchedule PreferNoSchedule NoExecute")
}

func TestBuildEnums(t *testing.T) {
	checkEnum[BuildType](t, BuildTypeValues, "full incremental specified single")
	checkEnum[BuildStage](t, BuildStageValues, "build publish")
}

func TestBuildEnumsJSON(t *testing.T) {
	value := struct {
		Type   BuildType       `json:"type"`
		Stage  BuildStage      `json:"stage"`
		Result SpecResultPhase `json:"result"`
	}{BuildTypeSingle, BuildStageBuild, SpecArchUnsupported}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"type":"single","stage":"build","result":"ArchUnsupported"}` {
		t.Fatalf("enum encoding: %s", data)
	}
}

func TestSpecResultPhase(t *testing.T) {
	checkEnum[SpecResultPhase](t, SpecResultPhaseValues,
		"Pending Prepared PrepareFailed Running BuildFailed RpmDependsMissing InstallFailed Succeeded ArchUnsupported")
	for _, tc := range []struct {
		phase          SpecResultPhase
		terminal       bool
		buildSucceeded bool
	}{
		{SpecPending, false, false},
		{SpecPrepared, false, false},
		{SpecPrepareFailed, true, false},
		{SpecRunning, false, false},
		{SpecBuildFailed, true, false},
		{SpecRpmDependsMissing, true, false},
		{SpecInstallFailed, true, true},
		{SpecSucceeded, true, true},
		{SpecArchUnsupported, true, false},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			if tc.phase.IsTerminal() != tc.terminal || tc.phase.BuildSucceeded() != tc.buildSucceeded {
				t.Fatalf("terminal=%t buildSucceeded=%t, want %t/%t",
					tc.phase.IsTerminal(), tc.phase.BuildSucceeded(), tc.terminal, tc.buildSucceeded)
			}
		})
	}
}

func TestJobResultStatus(t *testing.T) {
	for _, status := range []JobResultStatus{JobResultSucceeded, JobResultBuildFailed, JobResultInstallFailed} {
		if !status.IsValid() {
			t.Errorf("script result %q is not valid", status)
		}
	}
	if JobResultStatus("Unknown").IsValid() {
		t.Error("unknown script result is valid")
	}
}

func TestProjectPhase(t *testing.T) {
	checkEnum[ProjectPhase](t, ProjectPhaseValues, "Active Terminating")
}

func TestBuildInfoPhase(t *testing.T) {
	checkEnum[BuildInfoPhase](t, BuildInfoPhaseValues, "Pending Processing Completed Failed Aborted")
	checkTerminalPhases[BuildInfoPhase](t, BuildInfoPhaseValues(), "Completed Failed Aborted")
}

func TestJobPhase(t *testing.T) {
	checkEnum[JobPhase](t, JobPhaseValues, "Pending Running Succeeded Failed Aborted")
	checkTerminalPhases[JobPhase](t, JobPhaseValues(), "Succeeded Failed Aborted")
}

func TestJobStage(t *testing.T) {
	checkEnum[JobStage](t, JobStageValues, "Pending Running PostRun")
}

func TestRunnerPhase(t *testing.T) {
	checkEnum[RunnerPhase](t, RunnerPhaseValues, "Online Offline Evicted")
}

func TestSnapshotPhase(t *testing.T) {
	checkEnum[SnapshotPhase](t, SnapshotPhaseValues, "Pending Processing Active")
}

func TestBuildPhase(t *testing.T) {
	checkEnum[BuildPhase](t, BuildPhaseValues, "Pending Prepared Processing Success Failed Aborted")
	checkTerminalPhases[BuildPhase](t, BuildPhaseValues(), "Success Failed Aborted")
}

func TestRpmRepoReleasePhase(t *testing.T) {
	checkEnum[RpmRepoReleasePhase](t, RpmRepoReleasePhaseValues, "Pending Creating Ready Failed Skipped")
}
