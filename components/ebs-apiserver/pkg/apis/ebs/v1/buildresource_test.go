package v1

import "testing"

func TestResolveBuildResourcesMergesPartialOverrides(t *testing.T) {
	spec := BuildResourceSpec{
		Default: ResourceRequirements{
			Requests: map[string]string{"cpu": "4", "memory": "8Gi"},
		},
		Packages: map[string]PackageResourceConfig{
			"gcc": {
				Default: ResourceRequirements{
					Requests: map[string]string{"memory": "6Gi"},
				},
				Arches: map[string]ResourceRequirements{
					"riscv64": {
						Requests: map[string]string{"cpu": "3"},
					},
				},
			},
		},
	}

	got := ResolveBuildResources(spec, "gcc", "riscv64")
	if got.Requests["cpu"] != "3" || got.Requests["memory"] != "6Gi" ||
		got.Limits["cpu"] != "3" || got.Limits["memory"] != "6Gi" {
		t.Fatalf("unexpected effective resources: %#v", got)
	}

	fallback := ResolveBuildResources(spec, "bash", "aarch64")
	if fallback.Requests["cpu"] != "4" || fallback.Requests["memory"] != "8Gi" {
		t.Fatalf("unexpected table fallback: %#v", fallback)
	}
}
