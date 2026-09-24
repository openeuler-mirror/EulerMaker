package build

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// reconciler carries the state of a single Build round of reconciliation.
type reconciler struct {
	controller *Controller
	ctx        context.Context
	key        string
	project    string
	now        metav1.Time
	current    *ebsv1.Build
}

func (c *Controller) sync(ctx context.Context, key string) (controller.ReconcileResult, error) {
	project, name, ok := splitKey(key)
	if !ok {
		return controller.ReconcileResult{}, controller.NewPermanentError(fmt.Errorf("invalid Build key %q", key))
	}
	build, err := c.client.GetBuild(ctx, project, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if build.Status.Phase.IsTerminal() || build.DeletionTimestamp != nil {
		return controller.ReconcileResult{}, nil
	}
	if err := validatePhaseStage(build); err != nil {
		log.Printf("controller=%s key=%q uid=%q resourceVersion=%q phase=%q stage=%q reason=InvalidPhaseStage error=%v", Name, key, build.UID, build.ResourceVersion, build.Status.Phase, build.Status.Stage, err)
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
	r := &reconciler{
		controller: c,
		ctx:        ctx,
		key:        key,
		project:    project,
		now:        metav1.NewTime(c.clock.Now().UTC().Truncate(time.Second)),
		current:    build,
	}
	result, syncErr := r.run()
	if syncErr == nil {
		c.logReconciledAfterStart(build)
	}
	return result, syncErr
}

func (r *reconciler) run() (controller.ReconcileResult, error) {
	switch r.current.Status.Phase {
	case ebsv1.BuildPending:
		return r.pending()
	case ebsv1.BuildPrepared:
		return r.prepared()
	case ebsv1.BuildProcessing:
		if r.current.Status.Stage == stagePublish {
			return r.processingPublish()
		}
		return r.processingBuild()
	}
	return controller.ReconcileResult{}, controller.NewPermanentError(
		fmt.Errorf("Build %s/%s has an unsupported phase %q", r.current.Namespace, r.current.Name, r.current.Status.Phase))
}

// validatePhaseStage accepts only the phase/stage combinations the state machine can produce.
func validatePhaseStage(build *ebsv1.Build) error {
	phase, stage := build.Status.Phase, build.Status.Stage
	switch phase {
	case ebsv1.BuildPending, ebsv1.BuildPrepared:
		if stage == "" {
			return nil
		}
	case ebsv1.BuildProcessing:
		if stage == stageBuild {
			return nil
		}
		if stage == stagePublish && build.Spec.BuildType != buildTypeSingle {
			return nil
		}
	}
	return fmt.Errorf("Build %s/%s has an invalid phase/stage %q/%q", build.Namespace, build.Name, phase, stage)
}

func splitKey(key string) (string, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
