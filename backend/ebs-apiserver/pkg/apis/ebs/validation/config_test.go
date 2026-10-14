package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestValidateConfig(t *testing.T) {
	valid := func() *ebsv1.Config {
		return &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: "build-target"}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityPublic, Content: "targets: {}\n"}}
	}
	for _, tc := range []struct {
		name      string
		mutate    func(*ebsv1.Config)
		wantError bool
	}{
		{"valid", func(*ebsv1.Config) {}, false},
		{"business content is opaque", func(c *ebsv1.Config) { c.Spec.Content = "not: a target mapping" }, false},
		{"unknown name allowed", func(c *ebsv1.Config) { c.Name = "custom-config" }, false},
		{"missing name", func(c *ebsv1.Config) { c.Name = "" }, true},
		{"namespace", func(c *ebsv1.Config) { c.Namespace = "project" }, true},
		{"generateName", func(c *ebsv1.Config) { c.GenerateName = "cfg-" }, true},
		{"empty visibility", func(c *ebsv1.Config) { c.Spec.Visibility = "" }, true},
		{"unknown visibility", func(c *ebsv1.Config) { c.Spec.Visibility = "Private" }, true},
		{"empty content", func(c *ebsv1.Config) { c.Spec.Content = "" }, true},
		{"NUL", func(c *ebsv1.Config) { c.Spec.Content = "a\x00b" }, true},
		{"invalid UTF-8", func(c *ebsv1.Config) { c.Spec.Content = string([]byte{0xff}) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := valid()
			tc.mutate(obj)
			if got := len(ValidateConfig(obj)) != 0; got != tc.wantError {
				t.Fatalf("invalid=%v, errors=%v", got, ValidateConfig(obj))
			}
		})
	}
}
