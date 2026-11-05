package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// cleanupWriteLimit bounds abort and delete requests in a cleanup round.
const cleanupWriteLimit = 100

type cleanupJob struct {
	resource string
	ebsv1.Job
}

// cleanupChildren independently advances graph and Job cleanup after local production has stopped.
// Only Job cleanup depends on RpmRepo checkpoints; either branch may progress while the other waits or fails.
func (c *Controller) cleanupChildren(ctx context.Context, info *ebsv1.BuildInfo) (bool, error) {
	graphDone, graphErr := cleanupGraph(ctx, c.client, info)
	repoReady, err := checkCleanupRepository(ctx, c.client, info.Namespace, info.Name)
	if err != nil || !repoReady {
		return false, errors.Join(graphErr, err)
	}
	jobsDone, jobsErr := c.cleanupJobs(ctx, info)
	return graphDone && jobsDone, errors.Join(graphErr, jobsErr)
}

// checkCleanupRepository waits for the producer's CleanupReady acknowledgment.
func checkCleanupRepository(ctx context.Context, client Client, project, name string) (bool, error) {
	repo, err := client.GetRpmRepo(ctx, project, name)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return meta.IsStatusConditionTrue(repo.Status.Conditions, ebsv1.RpmRepoConditionCleanupReady), nil
}

// cleanupJobs scans from the start each round and waits for physical removal of Jobs and OldJobs.
func (c *Controller) cleanupJobs(ctx context.Context, info *ebsv1.BuildInfo) (bool, error) {
	jobs, err := listCleanupJobs(ctx, c.client, info)
	if err != nil {
		return false, err
	}
	if len(jobs) == 0 {
		return true, nil
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].resource != jobs[j].resource {
			return jobs[i].resource < jobs[j].resource
		}
		return jobs[i].Name < jobs[j].Name
	})
	var failures []error
	writes := 0
	for _, target := range jobs {
		if writes >= cleanupWriteLimit {
			break
		}
		job := &target.Job
		if job.Labels[ebsv1.JobBuildNameLabel] != info.Name {
			failures = append(failures, fmt.Errorf("unexpected %s/%s ownership", target.resource, target.Name))
			continue
		}
		if job.DeletionTimestamp != nil {
			continue
		}
		writes++
		if target.resource == "jobs" && !job.Status.Phase.IsTerminal() {
			_, err = c.client.AbortJob(ctx, job.Namespace, job.Name, job.UID, "BuildDeleted")
		} else {
			err = c.client.DeleteCleanupChild(ctx, info.Namespace, CleanupTarget{target.resource, job.Name}, job)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, err)
		}
	}
	return false, errors.Join(failures...)
}

// listCleanupJobs retains projected objects for conditional writes after both lists have completed.
func listCleanupJobs(ctx context.Context, client Client, info *ebsv1.BuildInfo) ([]cleanupJob, error) {
	selector := labels.SelectorFromSet(labels.Set{ebsv1.JobBuildNameLabel: info.Name})
	var targets []cleanupJob
	for _, resource := range []struct {
		name string
		list func(context.Context, string, labels.Selector, ...string) ([]ebsv1.Job, error)
	}{
		{"jobs", client.ListJobs},
		{"oldjobs", client.ListOldJobs},
	} {
		fields := []string{"metadata"}
		if resource.name == "jobs" {
			fields = append(fields, "status.phase")
		}
		jobs, err := resource.list(ctx, info.Namespace, selector, fields...)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			targets = append(targets, cleanupJob{resource.name, job})
		}
	}
	return targets, nil
}

// cleanupGraph verifies the BuildInfo owner UID before deleting its graph.
func cleanupGraph(ctx context.Context, client Client, info *ebsv1.BuildInfo) (bool, error) {
	project, name := info.Namespace, info.Name
	target := CleanupTarget{"buildgraphs", name}
	graph, err := client.GetBuildGraph(ctx, project, name, true)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	owner := metav1.GetControllerOf(graph)
	if owner == nil || owner.Kind != "BuildInfo" || owner.Name != name || owner.APIVersion != ebsv1.SchemeGroupVersion.String() || owner.UID != info.UID {
		return false, fmt.Errorf("BuildGraph owner does not match BuildInfo")
	}
	if graph.DeletionTimestamp == nil {
		err = client.DeleteCleanupChild(ctx, project, target, graph)
	}
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

func (c *Controller) cleanupBuildInfo(ctx context.Context, info *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	if !slices.Contains(info.Finalizers, ebsv1.BuildInfoCleanupFinalizer) {
		return controller.ReconcileResult{}, nil
	}
	wait := controller.ReconcileResult{RequeueAfter: time.Second}
	done, err := c.cleanupChildren(ctx, info)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if !done {
		return wait, nil
	}
	latest, err := c.client.GetBuildInfo(ctx, info.Namespace, info.Name)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if latest.UID != info.UID || latest.DeletionTimestamp == nil {
		return controller.ReconcileResult{}, nil
	}
	latest = latest.DeepCopy()
	latest.Finalizers = slices.DeleteFunc(latest.Finalizers, func(value string) bool { return value == ebsv1.BuildInfoCleanupFinalizer })
	if err := c.client.FinalizeBuildInfo(ctx, latest); err != nil {
		return controller.ReconcileResult{}, err
	}
	c.logf(info.Namespace+"/"+info.Name, "BuildInfoCleanupCompleted", "Jobs, OldJobs and BuildGraph removed")
	return controller.ReconcileResult{}, nil
}
