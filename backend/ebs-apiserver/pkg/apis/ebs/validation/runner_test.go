package validation

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func TestValidateRunnerTaints(t *testing.T) {
	for _, tc := range []struct {
		name      string
		taints    []ebsv1.RunnerTaint
		wantField string
		wantType  field.ErrorType
	}{
		{name: "no taints"},
		{name: "empty value", taints: []ebsv1.RunnerTaint{{Key: "dedicated", Effect: ebsv1.TaintEffectNoSchedule}}},
		{name: "same key different effects", taints: []ebsv1.RunnerTaint{
			{Key: "ebs.io/pool", Value: "gating", Effect: ebsv1.TaintEffectNoSchedule},
			{Key: "ebs.io/pool", Value: "gating", Effect: ebsv1.TaintEffectPreferNoSchedule},
			{Key: "ebs.io/pool", Value: "gating", Effect: ebsv1.TaintEffectNoExecute},
		}},
		{name: "maximum name and value", taints: []ebsv1.RunnerTaint{
			{Key: "ebs.io/" + strings.Repeat("a", 63), Value: strings.Repeat("b", 63), Effect: ebsv1.TaintEffectNoSchedule},
		}},
		{
			name:      "empty key",
			taints:    []ebsv1.RunnerTaint{{Effect: ebsv1.TaintEffectNoSchedule}},
			wantField: "spec.taints[0].key", wantType: field.ErrorTypeInvalid,
		},
		{
			name:      "invalid key prefix",
			taints:    []ebsv1.RunnerTaint{{Key: "Bad_Domain/pool", Effect: ebsv1.TaintEffectNoSchedule}},
			wantField: "spec.taints[0].key", wantType: field.ErrorTypeInvalid,
		},
		{
			name:      "long key name",
			taints:    []ebsv1.RunnerTaint{{Key: strings.Repeat("a", 64), Effect: ebsv1.TaintEffectNoSchedule}},
			wantField: "spec.taints[0].key", wantType: field.ErrorTypeInvalid,
		},
		{
			name:      "invalid value",
			taints:    []ebsv1.RunnerTaint{{Key: "pool", Value: "two words", Effect: ebsv1.TaintEffectNoSchedule}},
			wantField: "spec.taints[0].value", wantType: field.ErrorTypeInvalid,
		},
		{
			name:      "long value",
			taints:    []ebsv1.RunnerTaint{{Key: "pool", Value: strings.Repeat("a", 64), Effect: ebsv1.TaintEffectNoSchedule}},
			wantField: "spec.taints[0].value", wantType: field.ErrorTypeInvalid,
		},
		{
			name:      "unsupported effect",
			taints:    []ebsv1.RunnerTaint{{Key: "pool", Effect: "Other"}},
			wantField: "spec.taints[0].effect", wantType: field.ErrorTypeNotSupported,
		},
		{name: "duplicate", taints: []ebsv1.RunnerTaint{
			{Key: "pool", Value: "gating", Effect: ebsv1.TaintEffectNoSchedule},
			{Key: "pool", Value: "gating", Effect: ebsv1.TaintEffectNoSchedule},
		}, wantField: "spec.taints[1]", wantType: field.ErrorTypeDuplicate},
		{name: "same key effect different value", taints: []ebsv1.RunnerTaint{
			{Key: "pool", Value: "gating", Effect: ebsv1.TaintEffectNoSchedule},
			{Key: "pool", Value: "other", Effect: ebsv1.TaintEffectNoSchedule},
		}, wantField: "spec.taints[1]", wantType: field.ErrorTypeDuplicate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := validRunner("ct", "x86_64")
			old.Spec.Taints = []ebsv1.RunnerTaint{{Key: "old", Effect: ebsv1.TaintEffectNoSchedule}}
			runner := validRunner("ct", "x86_64")
			runner.Spec.Taints = tc.taints
			for operation, errs := range map[string]field.ErrorList{
				"create": ValidateRunner(runner), "update": ValidateRunnerUpdate(runner, old),
			} {
				t.Run(operation, func(t *testing.T) {
					want := map[string]field.ErrorType{}
					if tc.wantField != "" {
						want[tc.wantField] = tc.wantType
					}
					assertErrorList(t, errs, len(want), want)
				})
			}
		})
	}
}

func TestValidateRunner(t *testing.T) {
	tests := []struct {
		name       string
		runner     *ebsv1.Runner
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:   "valid ct",
			runner: validRunner("ct", "x86_64"),
		},
		{
			name:   "valid vm",
			runner: validRunner("vm", "x86_64"),
		},
		{
			name:   "valid hw",
			runner: validRunner("hw", "x86_64"),
		},
		{
			name:     "requires name type arch and instance id",
			runner:   &ebsv1.Runner{},
			wantErrs: 4,
			wantFields: map[string]field.ErrorType{
				"metadata.name":   field.ErrorTypeRequired,
				"spec.type":       field.ErrorTypeRequired,
				"spec.arch":       field.ErrorTypeRequired,
				"spec.instanceID": field.ErrorTypeRequired,
			},
		},
		{
			name:     "rejects unsupported type",
			runner:   validRunner("container", "x86_64"),
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.type": field.ErrorTypeNotSupported,
			},
		},
		{
			name: "rejects labels that do not match spec",
			runner: func() *ebsv1.Runner {
				runner := validRunner("ct", "x86_64")
				runner.Labels["ebs.io/runner-type"] = "vm"
				runner.Labels["ebs.io/runner-arch"] = "aarch64"
				return runner
			}(),
			wantErrs: 2,
			wantFields: map[string]field.ErrorType{
				"metadata.labels[ebs.io/runner-type]": field.ErrorTypeInvalid,
				"metadata.labels[ebs.io/runner-arch]": field.ErrorTypeInvalid,
			},
		},
		{
			name: "rejects non-v4 instance id",
			runner: &ebsv1.Runner{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-a", Labels: map[string]string{"ebs.io/runner-type": "ct", "ebs.io/runner-arch": "x86_64"}},
				Spec: ebsv1.RunnerSpec{
					InstanceID: "5d65d05e-37b6-1e7b-bfcb-264930f4436b",
					Type:       "ct",
					Arch:       "x86_64",
				},
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.instanceID": field.ErrorTypeInvalid,
			},
		},
		{
			name: "rejects uppercase instance id",
			runner: &ebsv1.Runner{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-a", Labels: map[string]string{"ebs.io/runner-type": "ct", "ebs.io/runner-arch": "x86_64"}},
				Spec: ebsv1.RunnerSpec{
					InstanceID: "5D65D05E-37B6-4E7B-BFCB-264930F4436B",
					Type:       "ct",
					Arch:       "x86_64",
				},
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.instanceID": field.ErrorTypeInvalid,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateRunner(tt.runner)
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateRunnerUpdate(t *testing.T) {
	tests := []struct {
		name       string
		newRunner  *ebsv1.Runner
		oldRunner  *ebsv1.Runner
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:      "valid unchanged fields",
			newRunner: validRunner("ct", "x86_64"),
			oldRunner: validRunner("ct", "x86_64"),
		},
		{
			name:      "type and arch may change",
			newRunner: validRunner("vm", "aarch64"),
			oldRunner: validRunner("ct", "x86_64"),
		},
		{
			name: "instance id is immutable",
			newRunner: &ebsv1.Runner{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-a", Labels: map[string]string{"ebs.io/runner-type": "ct", "ebs.io/runner-arch": "x86_64"}},
				Spec: ebsv1.RunnerSpec{
					InstanceID: "dc12a241-34c4-45b0-92cf-58ab1234b9c2",
					Type:       "ct",
					Arch:       "x86_64",
				},
			},
			oldRunner: validRunner("ct", "x86_64"),
			wantErrs:  1,
			wantFields: map[string]field.ErrorType{
				"spec.instanceID": field.ErrorTypeInvalid,
			},
		},
		{
			name:      "also validates new object",
			newRunner: &ebsv1.Runner{},
			oldRunner: validRunner("ct", "x86_64"),
			wantErrs:  5,
			wantFields: map[string]field.ErrorType{
				"spec.type":       field.ErrorTypeRequired,
				"spec.arch":       field.ErrorTypeRequired,
				"spec.instanceID": field.ErrorTypeRequired,
				"metadata.name":   field.ErrorTypeRequired,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateRunnerUpdate(tt.newRunner, tt.oldRunner)
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateRunnerStatusUpdate(t *testing.T) {
	tests := []struct {
		name       string
		phase      ebsv1.RunnerPhase
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{name: "allows online", phase: ebsv1.RunnerOnline},
		{name: "allows offline", phase: ebsv1.RunnerOffline},
		{name: "allows evicted", phase: ebsv1.RunnerEvicted},
		{
			name:     "rejects empty phase",
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.phase": field.ErrorTypeRequired,
			},
		},
		{
			name:     "rejects legacy phase",
			phase:    ebsv1.RunnerPhase("Running"),
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.phase": field.ErrorTypeNotSupported,
			},
		},
		{
			name:     "rejects unsupported phase",
			phase:    ebsv1.RunnerPhase("Unknown"),
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.phase": field.ErrorTypeNotSupported,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newRunner := validRunner("ct", "x86_64")
			newRunner.Status.Phase = tt.phase
			errs := ValidateRunnerStatusUpdate(newRunner, validRunner("ct", "x86_64"))
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func validRunner(runnerType, arch string) *ebsv1.Runner {
	return &ebsv1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-a", Labels: map[string]string{
			"ebs.io/runner-type": runnerType,
			"ebs.io/runner-arch": arch,
		}},
		Spec: ebsv1.RunnerSpec{
			InstanceID: "5d65d05e-37b6-4e7b-bfcb-264930f4436b",
			Type:       ebsv1.ExecutionType(runnerType),
			Arch:       arch,
		},
	}
}

func TestValidateRunnerActiveJobs(t *testing.T) {
	for _, count := range []int32{-1, 0, 2} {
		runner := validRunner("ct", "x86_64")
		runner.Status.Phase = ebsv1.RunnerOnline
		runner.Status.ActiveJobs = count
		errs := ValidateRunnerStatusUpdate(runner, runner)
		if count < 0 {
			if len(errs) != 1 || errs[0].Field != "status.activeJobs" {
				t.Fatalf("negative count errors: %v", errs)
			}
		} else if len(errs) != 0 {
			t.Fatalf("count %d rejected: %v", count, errs)
		}
	}
}
