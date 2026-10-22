package validation

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
)

func TestValidateBuildResourceContent(t *testing.T) {
	const defaults = "default: {requests: {cpu: '4', memory: 8Gi}}\n"
	for _, tc := range []struct {
		name, content string
		wantError     bool
	}{
		{"defaults", defaults, false},
		{"package override", defaults + "packages: {gcc: {default: {requests: {cpu: '8'}}}}", false},
		{"architecture override", defaults + "packages: {'kernel:kernel-rt': {arches: {aarch64: {requests: {memory: 16Gi}}}}}", false},
		{"limit override", defaults + "packages: {gcc: {default: {limits: {cpu: '8'}}}}", false},
		{"missing defaults", "{}", true},
		{"missing memory", "default: {requests: {cpu: '4'}}", true},
		{"invalid quantity", "default: {requests: {cpu: many, memory: 8Gi}}", true},
		{"zero quantity", "default: {requests: {cpu: '0', memory: 8Gi}}", true},
		{"negative quantity", defaults + "packages: {gcc: {default: {requests: {cpu: '-1'}}}}", true},
		{"unsupported resource", defaults + "packages: {gcc: {default: {requests: {gpu: '1'}}}}", true},
		{"empty package rule", defaults + "packages: {gcc: {}}", true},
		{"invalid package name", defaults + "packages: {'bad/name': {default: {requests: {cpu: '8'}}}}", true},
		{"invalid architecture", defaults + "packages: {gcc: {arches: {'bad/arch': {requests: {cpu: '8'}}}}}", true},
		{"default limit below request", "default: {requests: {cpu: '4', memory: 8Gi}, limits: {cpu: '2'}}", true},
		{"inherited request above limit", defaults + "packages: {gcc: {default: {limits: {cpu: '2'}}}}", true},
		{"architecture limit below package request", defaults + "packages: {gcc: {default: {requests: {cpu: '8'}}, arches: {aarch64: {limits: {cpu: '6'}}}}}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var content ebsv1.BuildResourceContent
			if err := yaml.UnmarshalStrict([]byte(tc.content), &content); err != nil {
				t.Fatal(err)
			}
			before := content.DeepCopy()
			if errs := validateBuildResourceContent(&content); (len(errs) != 0) != tc.wantError {
				t.Fatalf("errors=%v, wantError=%v", errs, tc.wantError)
			}
			if !reflect.DeepEqual(&content, before) {
				t.Fatal("validation modified content")
			}
			obj := &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: ebsv1.BuildResourceConfigName}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityOpsOnly, Content: tc.content}}
			if errs := ValidateConfig(obj); (len(errs) != 0) != tc.wantError {
				t.Fatalf("Config errors=%v", errs)
			}
		})
	}
}

func TestBuildResourceConfigStrictDecoding(t *testing.T) {
	for _, content := range []string{
		"default: [",
		"default: {requests: {cpu: '4', memory: 8Gi}, unexpected: true}",
		"default: {requests: {cpu: '4', cpu: '8', memory: 8Gi}}",
		"default: {requests: {cpu: '4', memory: 8Gi}}\npackages: {gcc: {}, gcc: {}}",
	} {
		obj := &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: ebsv1.BuildResourceConfigName}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityOpsOnly, Content: content}}
		if errs := ValidateConfig(obj); len(errs) != 1 || errs[0].Field != "spec.content" {
			t.Fatalf("errors=%v", errs)
		}
	}
}

func TestValidateServiceConfigContent(t *testing.T) {
	valid := func() *ebsv1.ServiceConfigContent {
		return &ebsv1.ServiceConfigContent{
			GitServer: ebsv1.GitServerConfig{InternalURL: "http://git-server:8080"},
			ArtifactManagers: map[string]ebsv1.ArtifactManagerInstance{
				"default": {InternalURL: "http://artifact-manager:8081", RunnerInternalURL: "http://127.0.0.1:8081"},
				"art-01":  {InternalURL: "https://artifact-manager.art-01:8081", RunnerExternalURL: "https://artifacts.example.com"},
			},
		}
	}
	for _, tc := range []struct {
		name      string
		change    func(*ebsv1.ServiceConfigContent)
		wantField string
	}{
		{"valid", func(*ebsv1.ServiceConfigContent) {}, ""},
		{"Git HTTPS with prefix", func(o *ebsv1.ServiceConfigContent) {
			o.GitServer.InternalURL = "https://git-server:8443/prefix/"
		}, ""},
		{"missing Git URL", func(o *ebsv1.ServiceConfigContent) {
			o.GitServer.InternalURL = ""
		}, "spec.content.gitServer.internalURL"},
		{"external HTTP", func(o *ebsv1.ServiceConfigContent) {
			entry := o.ArtifactManagers["art-01"]
			entry.RunnerExternalURL = "http://external:8081"
			o.ArtifactManagers["art-01"] = entry
		}, ""},
		{"no default instance", func(o *ebsv1.ServiceConfigContent) { delete(o.ArtifactManagers, "default") }, ""},
		{"IPv6 and path prefix", func(o *ebsv1.ServiceConfigContent) {
			o.ArtifactManagers["art-02"] = ebsv1.ArtifactManagerInstance{InternalURL: "http://[::1]:8081/prefix/"}
		}, ""},
		{"empty instances", func(o *ebsv1.ServiceConfigContent) { o.ArtifactManagers = nil }, "spec.content.artifactManagers"},
		{"invalid name", func(o *ebsv1.ServiceConfigContent) {
			o.ArtifactManagers["Art/01"] = ebsv1.ArtifactManagerInstance{InternalURL: "http://host"}
		}, "spec.content.artifactManagers[Art/01]"},
		{"missing internal URL", func(o *ebsv1.ServiceConfigContent) { o.ArtifactManagers["unused"] = ebsv1.ArtifactManagerInstance{} }, "spec.content.artifactManagers[unused].internalURL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := valid()
			tc.change(obj)
			errs := validateServiceConfigContent(obj)
			if tc.wantField == "" && len(errs) != 0 {
				t.Fatalf("errors=%v", errs)
			}
			if tc.wantField != "" && (len(errs) != 1 || errs[0].Field != tc.wantField) {
				t.Fatalf("errors=%v, wantField=%s", errs, tc.wantField)
			}
			content, err := json.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			config := &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: ebsv1.ServiceConfigName}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityOpsOnly, Content: string(content)}}
			if errs := ValidateConfig(config); (len(errs) != 0) != (tc.wantField != "") {
				t.Fatalf("Config errors=%v", errs)
			}
		})
	}
	for _, tc := range []struct{ name, field, value string }{
		{"relative", "internalURL", "/artifacts"},
		{"scheme", "internalURL", "ftp://host"},
		{"credentials", "internalURL", "https://user:secret@host"},
		{"query", "internalURL", "https://host?token=secret"},
		{"empty query", "internalURL", "https://host?"},
		{"fragment", "internalURL", "https://host#fragment"},
		{"empty fragment", "internalURL", "https://host#"},
		{"whitespace", "internalURL", " http://host"},
		{"missing hostname", "internalURL", "http://:8081"},
		{"invalid port", "internalURL", "http://host:65536"},
		{"empty port", "internalURL", "http://host:"},
		{"zero port", "internalURL", "http://host:0"},
		{"unicode whitespace", "internalURL", "http://host/path\u00a0"},
		{"invalid runner URL", "runnerInternalURL", "relative"},
		{"external unsupported scheme", "runnerExternalURL", "ftp://external"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := valid()
			entry := obj.ArtifactManagers["art-01"]
			switch tc.field {
			case "internalURL":
				entry.InternalURL = tc.value
			case "runnerInternalURL":
				entry.RunnerInternalURL = tc.value
			case "runnerExternalURL":
				entry.RunnerExternalURL = tc.value
			}
			obj.ArtifactManagers["art-01"] = entry
			errs := validateServiceConfigContent(obj)
			if len(errs) != 1 || errs[0].Field != "spec.content.artifactManagers[art-01]."+tc.field {
				t.Fatalf("errors=%v", errs)
			}
			obj = valid()
			obj.GitServer.InternalURL = tc.value
			errs = validateServiceConfigContent(obj)
			if len(errs) != 1 || errs[0].Field != "spec.content.gitServer.internalURL" {
				t.Fatalf("Git Server errors=%v", errs)
			}
		})
	}
}

func TestServiceConfigStrictDecoding(t *testing.T) {
	for _, content := range []string{
		"artifactManagers: [",
		"artifactManagers: {default: {internalURL: 'http://host', secret: hidden}}",
		"artifactManagers: {default: {internalURL: 'http://host', internalURL: 'http://other'}}",
		"artifactManagers: {default: {internalURL: 'http://host'}, default: {internalURL: 'http://other'}}",
		"gitServer: {internalURL: 'http://git', secret: hidden}",
		"gitServer: {internalURL: 'http://git', internalURL: 'http://other'}",
		"gitServer: {internalURL: 'http://git'}\ngitServer: {internalURL: 'http://other'}",
	} {
		obj := &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: ebsv1.ServiceConfigName}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityOpsOnly, Content: content}}
		if errs := ValidateConfig(obj); len(errs) != 1 || errs[0].Field != "spec.content" {
			t.Fatalf("errors=%v", errs)
		}
	}
}

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
		{"invalid target content", func(c *ebsv1.Config) { c.Spec.Content = "not: a target mapping" }, true},
		{"custom content is opaque", func(c *ebsv1.Config) { c.Name = "custom-config"; c.Spec.Content = "custom content" }, false},
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
			if errs := validateBuildTargetContent(o); (len(errs) > 0) != test.invalid {
				t.Fatalf("errors=%v", errs)
			}
			content, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			obj := &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: ebsv1.BuildTargetConfigName}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityPublic, Content: string(content)}}
			if errs := ValidateConfig(obj); (len(errs) > 0) != test.invalid {
				t.Fatalf("Config errors=%v", errs)
			}
		})
	}
	for _, image := range []string{"", "https://registry/repo:v1", "user:pass@registry/repo:v1", "repo:bad tag", " repo:v1", "repo@sha256:bad"} {
		o := valid()
		o.Targets["openEuler-24.03-LTS"].Arches["x86_64"] = ebsv1.BuildTargetArch{Image: image}
		if len(validateBuildTargetContent(o)) == 0 {
			t.Errorf("accepted invalid image %q", image)
		}
	}
}

func TestBuildTargetConfigStrictDecoding(t *testing.T) {
	for _, content := range []string{
		"targets: [",
		"targets: {}\nunknown: true",
		"targets: {os: {arches: {x86_64: {image: build:v1, image: build:v2}}}}",
	} {
		obj := &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: ebsv1.BuildTargetConfigName}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityPublic, Content: content}}
		if errs := ValidateConfig(obj); len(errs) != 1 || errs[0].Field != "spec.content" {
			t.Fatalf("errors=%v", errs)
		}
	}
}
