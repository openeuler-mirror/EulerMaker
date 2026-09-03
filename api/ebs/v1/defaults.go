package v1

func SetDefaults_Project(obj *Project) {
	if len(obj.Spec.DisplayName) == 0 {
		obj.Spec.DisplayName = obj.Name
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
	if len(obj.Spec.Runtime) == 0 {
		obj.Spec.Runtime = "ct"
	}
	if obj.Spec.TimeoutSeconds == 0 {
		obj.Spec.TimeoutSeconds = 10800
	}
}

func SetDefaults_Runner(obj *Runner) {
	if len(obj.Spec.Type) == 0 {
		obj.Spec.Type = "ct"
	}
}
