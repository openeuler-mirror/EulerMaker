package validation

import (
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestBuildTargetContentValidation(t *testing.T) {
	valid := func() *ebsv1.BuildTargetContent {
		return &ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{
			"openEuler-24.03-LTS": {Arches: map[string]ebsv1.BuildTargetArch{"x86_64": {Image: "registry.example:5000/team/build:v1"}, "riscv64": {Image: "build@sha256:" + strings.Repeat("a", 64)}}},
		}}
	}
	for _, test := range []struct {
		name    string
		change  func(*ebsv1.BuildTargetContent)
		invalid bool
	}{
		{"valid", func(*ebsv1.BuildTargetContent) {}, false},
		{"empty", func(o *ebsv1.BuildTargetContent) { o.Targets = map[string]ebsv1.BuildTargetConfigEntry{} }, false},
		{"null", func(o *ebsv1.BuildTargetContent) { o.Targets = nil }, true},
		{"os", func(o *ebsv1.BuildTargetContent) { o.Targets["bad/os"] = ebsv1.BuildTargetConfigEntry{} }, true},
		{"arches", func(o *ebsv1.BuildTargetContent) { o.Targets["empty"] = ebsv1.BuildTargetConfigEntry{} }, true},
		{"arch", func(o *ebsv1.BuildTargetContent) {
			o.Targets["openEuler-24.03-LTS"].Arches["bad/arch"] = ebsv1.BuildTargetArch{Image: "build:v1"}
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := valid()
			test.change(o)
			if errs := ValidateBuildTargetContent(o); (len(errs) > 0) != test.invalid {
				t.Fatalf("errors=%v", errs)
			}
		})
	}
	for _, image := range []string{"", "https://registry/repo:v1", "user:pass@registry/repo:v1", "repo:bad tag", " repo:v1", "repo@sha256:bad"} {
		o := valid()
		o.Targets["openEuler-24.03-LTS"].Arches["x86_64"] = ebsv1.BuildTargetArch{Image: image}
		if len(ValidateBuildTargetContent(o)) == 0 {
			t.Errorf("accepted invalid image %q", image)
		}
	}
}
