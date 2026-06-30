package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *BuildTargetFlags) DeepCopyInto(out *BuildTargetFlags) {
	*out = *in
}

func (in *BuildTargetFlags) DeepCopy() *BuildTargetFlags {
	if in == nil {
		return nil
	}
	out := new(BuildTargetFlags)
	in.DeepCopyInto(out)
	return out
}

func (in *BuildTarget) DeepCopyInto(out *BuildTarget) {
	*out = *in
	if in.GroundProjects != nil {
		in, out := &in.GroundProjects, &out.GroundProjects
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	in.Flags.DeepCopyInto(&out.Flags)
}

func (in *BuildTarget) DeepCopy() *BuildTarget {
	if in == nil {
		return nil
	}
	out := new(BuildTarget)
	in.DeepCopyInto(out)
	return out
}

func (in *PackageRepo) DeepCopyInto(out *PackageRepo) {
	*out = *in
}

func (in *PackageRepo) DeepCopy() *PackageRepo {
	if in == nil {
		return nil
	}
	out := new(PackageRepo)
	in.DeepCopyInto(out)
	return out
}

func (in *ProjectSpec) DeepCopyInto(out *ProjectSpec) {
	*out = *in
	if in.BuildTargets != nil {
		in, out := &in.BuildTargets, &out.BuildTargets
		*out = make([]BuildTarget, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.PackageRepos != nil {
		in, out := &in.PackageRepos, &out.PackageRepos
		*out = make([]PackageRepo, len(*in))
		copy(*out, *in)
	}
}

func (in *ProjectSpec) DeepCopy() *ProjectSpec {
	if in == nil {
		return nil
	}
	out := new(ProjectSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ProjectStatus) DeepCopyInto(out *ProjectStatus) {
	*out = *in
	in.LastBuildTime.DeepCopyInto(&out.LastBuildTime)
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *ProjectStatus) DeepCopy() *ProjectStatus {
	if in == nil {
		return nil
	}
	out := new(ProjectStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *Project) DeepCopyInto(out *Project) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Project) DeepCopy() *Project {
	if in == nil {
		return nil
	}
	out := new(Project)
	in.DeepCopyInto(out)
	return out
}

func (in *Project) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ProjectList) DeepCopyInto(out *ProjectList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Project, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *ProjectList) DeepCopy() *ProjectList {
	if in == nil {
		return nil
	}
	out := new(ProjectList)
	in.DeepCopyInto(out)
	return out
}

func (in *ProjectList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SpecCommit) DeepCopyInto(out *SpecCommit) {
	*out = *in
}

func (in *SpecCommit) DeepCopy() *SpecCommit {
	if in == nil {
		return nil
	}
	out := new(SpecCommit)
	in.DeepCopyInto(out)
	return out
}

func (in *SnapshotSpec) DeepCopyInto(out *SnapshotSpec) {
	*out = *in
	if in.SpecCommits != nil {
		in, out := &in.SpecCommits, &out.SpecCommits
		*out = make(map[string]SpecCommit, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
	if in.BuildTargets != nil {
		in, out := &in.BuildTargets, &out.BuildTargets
		*out = make([]BuildTarget, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.GroundProjects != nil {
		in, out := &in.GroundProjects, &out.GroundProjects
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
}

func (in *SnapshotSpec) DeepCopy() *SnapshotSpec {
	if in == nil {
		return nil
	}
	out := new(SnapshotSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *SnapshotStatus) DeepCopyInto(out *SnapshotStatus) {
	*out = *in
	in.StartTime.DeepCopyInto(&out.StartTime)
}

func (in *SnapshotStatus) DeepCopy() *SnapshotStatus {
	if in == nil {
		return nil
	}
	out := new(SnapshotStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *Snapshot) DeepCopyInto(out *Snapshot) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Snapshot) DeepCopy() *Snapshot {
	if in == nil {
		return nil
	}
	out := new(Snapshot)
	in.DeepCopyInto(out)
	return out
}

func (in *Snapshot) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SnapshotList) DeepCopyInto(out *SnapshotList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Snapshot, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *SnapshotList) DeepCopy() *SnapshotList {
	if in == nil {
		return nil
	}
	out := new(SnapshotList)
	in.DeepCopyInto(out)
	return out
}

func (in *SnapshotList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *BuildSpec) DeepCopyInto(out *BuildSpec) {
	*out = *in
	in.BuildTarget.DeepCopyInto(&out.BuildTarget)
	if in.Packages != nil {
		in, out := &in.Packages, &out.Packages
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
}

func (in *BuildSpec) DeepCopy() *BuildSpec {
	if in == nil {
		return nil
	}
	out := new(BuildSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *PackageStatus) DeepCopyInto(out *PackageStatus) {
	*out = *in
	in.StartTime.DeepCopyInto(&out.StartTime)
	in.EndTime.DeepCopyInto(&out.EndTime)
}

func (in *PackageStatus) DeepCopy() *PackageStatus {
	if in == nil {
		return nil
	}
	out := new(PackageStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *BuildStatus) DeepCopyInto(out *BuildStatus) {
	*out = *in
	in.StartTime.DeepCopyInto(&out.StartTime)
	in.EndTime.DeepCopyInto(&out.EndTime)
	if in.PackageStatus != nil {
		in, out := &in.PackageStatus, &out.PackageStatus
		*out = make(map[string]PackageStatus, len(*in))
		for k, v := range *in {
			(*out)[k] = *v.DeepCopy()
		}
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *BuildStatus) DeepCopy() *BuildStatus {
	if in == nil {
		return nil
	}
	out := new(BuildStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *Build) DeepCopyInto(out *Build) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Build) DeepCopy() *Build {
	if in == nil {
		return nil
	}
	out := new(Build)
	in.DeepCopyInto(out)
	return out
}

func (in *Build) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *BuildList) DeepCopyInto(out *BuildList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Build, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *BuildList) DeepCopy() *BuildList {
	if in == nil {
		return nil
	}
	out := new(BuildList)
	in.DeepCopyInto(out)
	return out
}

func (in *BuildList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *JobSpec) DeepCopyInto(out *JobSpec) {
	*out = *in
	in.ImageConfig.DeepCopyInto(&out.ImageConfig)
	if in.Env != nil {
		in, out := &in.Env, &out.Env
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
	if in.Commands != nil {
		in, out := &in.Commands, &out.Commands
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
}

func (in *JobSpec) DeepCopy() *JobSpec {
	if in == nil {
		return nil
	}
	out := new(JobSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *JobStatus) DeepCopyInto(out *JobStatus) {
	*out = *in
	in.StartTime.DeepCopyInto(&out.StartTime)
	in.EndTime.DeepCopyInto(&out.EndTime)
}

func (in *JobStatus) DeepCopy() *JobStatus {
	if in == nil {
		return nil
	}
	out := new(JobStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *Job) DeepCopyInto(out *Job) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Job) DeepCopy() *Job {
	if in == nil {
		return nil
	}
	out := new(Job)
	in.DeepCopyInto(out)
	return out
}

func (in *Job) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *JobList) DeepCopyInto(out *JobList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Job, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *JobList) DeepCopy() *JobList {
	if in == nil {
		return nil
	}
	out := new(JobList)
	in.DeepCopyInto(out)
	return out
}

func (in *JobList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
func (in *RunnerTaint) DeepCopyInto(out *RunnerTaint) {
	*out = *in
}

func (in *RunnerTaint) DeepCopy() *RunnerTaint {
	if in == nil {
		return nil
	}
	out := new(RunnerTaint)
	in.DeepCopyInto(out)
	return out
}

func (in *RunnerSpec) DeepCopyInto(out *RunnerSpec) {
	*out = *in
	if in.Taints != nil {
		in, out := &in.Taints, &out.Taints
		*out = make([]RunnerTaint, len(*in))
		copy(*out, *in)
	}
}

func (in *RunnerSpec) DeepCopy() *RunnerSpec {
	if in == nil {
		return nil
	}
	out := new(RunnerSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RunnerStatus) DeepCopyInto(out *RunnerStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.Capacity != nil {
		in, out := &in.Capacity, &out.Capacity
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
	if in.Allocatable != nil {
		in, out := &in.Allocatable, &out.Allocatable
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
	if in.Addresses != nil {
		in, out := &in.Addresses, &out.Addresses
		*out = make([]RunnerAddress, len(*in))
		copy(*out, *in)
	}
	in.Heartbeat.DeepCopyInto(&out.Heartbeat)
}

func (in *RunnerStatus) DeepCopy() *RunnerStatus {
	if in == nil {
		return nil
	}
	out := new(RunnerStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *RunnerAddress) DeepCopyInto(out *RunnerAddress) {
	*out = *in
}

func (in *RunnerAddress) DeepCopy() *RunnerAddress {
	if in == nil {
		return nil
	}
	out := new(RunnerAddress)
	in.DeepCopyInto(out)
	return out
}

func (in *RunnerInfo) DeepCopyInto(out *RunnerInfo) {
	*out = *in
}

func (in *RunnerInfo) DeepCopy() *RunnerInfo {
	if in == nil {
		return nil
	}
	out := new(RunnerInfo)
	in.DeepCopyInto(out)
	return out
}

func (in *Runner) DeepCopyInto(out *Runner) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Runner) DeepCopy() *Runner {
	if in == nil {
		return nil
	}
	out := new(Runner)
	in.DeepCopyInto(out)
	return out
}

func (in *Runner) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RunnerList) DeepCopyInto(out *RunnerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Runner, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *RunnerList) DeepCopy() *RunnerList {
	if in == nil {
		return nil
	}
	out := new(RunnerList)
	in.DeepCopyInto(out)
	return out
}

func (in *RunnerList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
