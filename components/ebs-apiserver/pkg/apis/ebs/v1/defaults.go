package v1

import (
	"regexp"
	"strings"
)

var (
	invalidNameChars  = regexp.MustCompile(`[^a-z0-9\-\.]`)
	consecutiveDashes = regexp.MustCompile(`-{2,}`)
)

func normalizeProjectName(name string) string {
	if len(name) == 0 {
		return name
	}
	normalized := strings.ToLower(name)
	normalized = invalidNameChars.ReplaceAllString(normalized, "-")
	normalized = strings.ReplaceAll(normalized, ".", "-")
	normalized = consecutiveDashes.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")
	if len(normalized) == 0 {
		return "project"
	}
	if len(normalized) > 63 {
		normalized = normalized[:63]
	}
	normalized = strings.TrimRight(normalized, "-")
	return normalized
}

func SetDefaults_Project(obj *Project) {
	if len(obj.Spec.DisplayName) == 0 {
		obj.Spec.DisplayName = obj.Name
	}
	if len(obj.Name) > 0 {
		obj.Name = normalizeProjectName(obj.Name)
	}
	if len(obj.Spec.SpecBranch) == 0 {
		obj.Spec.SpecBranch = "master"
	}
}

func SetDefaults_Build(obj *Build) {
	if len(obj.Spec.BuildType) == 0 {
		obj.Spec.BuildType = "full"
	}
}

func SetDefaults_Job(obj *Job) {
	if obj.Spec.Runtime == 0 {
		obj.Spec.Runtime = 10800
	}
}

func SetDefaults_Runner(obj *Runner) {
	if len(obj.Spec.Type) == 0 {
		obj.Spec.Type = "dc"
	}
}
