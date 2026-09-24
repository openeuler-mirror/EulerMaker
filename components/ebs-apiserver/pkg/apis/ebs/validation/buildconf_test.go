package validation

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestBuildConfValidation(t *testing.T) {
	valid := func() *ebsv1.BuildConf {
		return &ebsv1.BuildConf{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: ebsv1.BuildConfSpec{Targets: map[string]ebsv1.BuildConfTarget{
			"openEuler-24.03-LTS": {Arches: map[string]ebsv1.BuildConfArch{"x86_64": {Image: "registry.example:5000/team/build:v1"}, "riscv64": {Image: "build@sha256:" + strings.Repeat("a", 64)}}},
		}}}
	}
	for _, test := range []struct {
		name    string
		change  func(*ebsv1.BuildConf)
		invalid bool
	}{
		{"valid", func(*ebsv1.BuildConf) {}, false},
		{"empty", func(o *ebsv1.BuildConf) { o.Spec.Targets = map[string]ebsv1.BuildConfTarget{} }, false},
		{"null", func(o *ebsv1.BuildConf) { o.Spec.Targets = nil }, true},
		{"name", func(o *ebsv1.BuildConf) { o.Name = "other" }, true},
		{"namespace", func(o *ebsv1.BuildConf) { o.Namespace = "default" }, true},
		{"generate", func(o *ebsv1.BuildConf) { o.GenerateName = "conf-" }, true},
		{"os", func(o *ebsv1.BuildConf) { o.Spec.Targets["bad/os"] = ebsv1.BuildConfTarget{} }, true},
		{"arches", func(o *ebsv1.BuildConf) { o.Spec.Targets["empty"] = ebsv1.BuildConfTarget{} }, true},
		{"arch", func(o *ebsv1.BuildConf) {
			o.Spec.Targets["openEuler-24.03-LTS"].Arches["bad/arch"] = ebsv1.BuildConfArch{Image: "build:v1"}
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := valid()
			test.change(o)
			if errs := ValidateBuildConf(o); (len(errs) > 0) != test.invalid {
				t.Fatalf("errors=%v", errs)
			}
		})
	}
	for _, image := range []string{"", "https://registry/repo:v1", "user:pass@registry/repo:v1", "repo:bad tag", " repo:v1", "repo@sha256:bad"} {
		o := valid()
		o.Spec.Targets["openEuler-24.03-LTS"].Arches["x86_64"] = ebsv1.BuildConfArch{Image: image}
		if len(ValidateBuildConf(o)) == 0 {
			t.Errorf("accepted invalid image %q", image)
		}
	}
}
