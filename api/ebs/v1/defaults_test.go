package v1

import (
	"encoding/json"
	"testing"
)

func TestBuildPublishDefault(t *testing.T) {
	for _, buildType := range []BuildType{BuildTypeFull, BuildTypeIncremental, BuildTypeSpecified, BuildTypeSingle} {
		for _, value := range []string{"omitted", "true", "false"} {
			t.Run(string(buildType)+"/"+value, func(t *testing.T) {
				build := &Build{Spec: BuildSpec{BuildType: buildType}}
				if value != "omitted" {
					publish := value == "true"
					build.Spec.Publish = &publish
				}
				SetDefaults_Build(build)
				SetDefaults_Build(build)
				if value == "omitted" && buildType == BuildTypeSingle {
					if build.Spec.Publish != nil {
						t.Fatal("single build received a publish default")
					}
					return
				}
				if build.Spec.Publish == nil || *build.Spec.Publish != (value != "false") {
					t.Fatalf("publish = %v for %s", build.Spec.Publish, value)
				}
			})
		}
	}
}

func TestProjectTypeDefault(t *testing.T) {
	for _, value := range []string{"missing", "personal", "community", "", "invalid"} {
		p := &Project{}
		want := value
		if value == "missing" {
			want = ProjectTypePersonal
		} else {
			p.Labels = map[string]string{ProjectTypeLabel: value}
		}
		SetDefaults_Project(p)
		if p.Labels[ProjectTypeLabel] != want {
			t.Fatalf("%q defaulted to %q", value, p.Labels[ProjectTypeLabel])
		}
	}
}

func TestProjectPackageRefs(t *testing.T) {
	for _, ref := range []string{"", "refs/heads/main", "refs/tags/v1", "refs/merge-requests/501/head"} {
		t.Run(ref, func(t *testing.T) {
			project := &Project{Spec: ProjectSpec{PackageRepos: []PackageRepo{{Name: "gcc", Ref: ref}}}}
			for _, defaultRef := range []string{"", "refs/heads/another-branch", "refs/tags/v2"} {
				project.Spec.DefaultRef = defaultRef
				SetDefaults_Project(project)
				if got := project.Spec.PackageRepos[0].Ref; got != ref {
					t.Fatalf("package ref = %q, want %q with defaultRef %q", got, ref, defaultRef)
				}
			}
		})
	}
}

func TestProjectDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, input, displayName, ref string
	}{
		{"omitted", `{}`, "Invalid_Name", "refs/heads/master"},
		{"null ref", `{"defaultRef":null}`, "Invalid_Name", "refs/heads/master"},
		{"empty ref", `{"defaultRef":""}`, "Invalid_Name", "refs/heads/master"},
		{"explicit", `{"defaultRef":"refs/tags/v1","displayName":"My project"}`, "My project", "refs/tags/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var project Project
			project.Name = "Invalid_Name"
			if err := json.Unmarshal([]byte(tc.input), &project.Spec); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				SetDefaults_Project(&project)
				if project.Name != "Invalid_Name" || project.Spec.DisplayName != tc.displayName || project.Spec.DefaultRef != tc.ref {
					t.Fatalf("name=%q displayName=%q defaultRef=%q", project.Name, project.Spec.DisplayName, project.Spec.DefaultRef)
				}
			}
		})
	}
}

func TestJobDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   JobSpec
		runtime ExecutionType
		timeout int64
	}{
		{"omitted", JobSpec{}, ExecutionTypeContainer, 10800},
		{"explicit", JobSpec{Runtime: ExecutionTypeVM, TimeoutSeconds: 600}, ExecutionTypeVM, 600},
		{"runtime only", JobSpec{Runtime: ExecutionTypeHardware}, ExecutionTypeHardware, 10800},
		{"timeout only", JobSpec{TimeoutSeconds: 120}, ExecutionTypeContainer, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &Job{Spec: tc.input}
			for range 2 {
				SetDefaults_Job(job)
				if job.Spec.Runtime != tc.runtime || job.Spec.TimeoutSeconds != tc.timeout {
					t.Fatalf("runtime=%q timeout=%d, want %q/%d", job.Spec.Runtime, job.Spec.TimeoutSeconds, tc.runtime, tc.timeout)
				}
			}
		})
	}
}

func TestRunnerDefaults(t *testing.T) {
	for _, tc := range []struct {
		input, want ExecutionType
	}{
		{"", ExecutionTypeContainer},
		{ExecutionTypeContainer, ExecutionTypeContainer},
		{ExecutionTypeVM, ExecutionTypeVM},
		{ExecutionTypeHardware, ExecutionTypeHardware},
	} {
		t.Run(string(tc.input), func(t *testing.T) {
			runner := &Runner{Spec: RunnerSpec{Type: tc.input}}
			for range 2 {
				SetDefaults_Runner(runner)
				if runner.Spec.Type != tc.want {
					t.Fatalf("type=%q, want %q", runner.Spec.Type, tc.want)
				}
			}
		})
	}
}
