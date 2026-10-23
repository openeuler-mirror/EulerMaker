package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

const conditionBuildGraphUnavailable = "BuildGraphUnavailable"

type graphCacheEntry struct {
	ownerUID types.UID
	metadata metav1.ObjectMeta
	dcg      *DcgDict
}

func newBuildGraph(info *ebsv1.BuildInfo, nodes map[string]ebsv1.DcgNodeState) *ebsv1.BuildGraph {
	return &ebsv1.BuildGraph{
		TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "BuildGraph"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: info.Namespace, Name: info.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(info, ebsv1.SchemeGroupVersion.WithKind("BuildInfo")),
			},
		},
		Status: ebsv1.BuildGraphStatus{Nodes: nodes},
	}
}

// loadGraph validates the persisted version before reusing a cached graph. A nil result without a stop permits creation.
func (c *Controller) loadGraph(ctx context.Context, round *reconcileRound) (*DcgDict, bool, controller.ReconcileResult, error) {
	cached, hit := c.dcgDict.Get(round.key)
	meta, err := c.client.GetBuildGraph(ctx, round.current.Namespace, round.current.Name, true)
	if errors.Is(err, ErrNotFound) {
		if !(hit && cached.ownerUID == round.current.UID) && firstGraphState(round.current) {
			jobs, listErr := c.listRoundJobs(ctx, round)
			if listErr != nil {
				return nil, true, controller.ReconcileResult{}, listErr
			}
			if len(jobs) == 0 {
				return nil, false, controller.ReconcileResult{}, nil
			}
		}
		result, writeErr := c.graphUnavailable(ctx, round, "BuildGraphMissing")
		return nil, true, result, writeErr
	}
	if err != nil {
		return nil, true, controller.ReconcileResult{}, graphReadError(err)
	}
	if reason := graphIdentityReason(meta, round.current); reason != "" {
		result, writeErr := c.graphUnavailable(ctx, round, reason)
		return nil, true, result, writeErr
	}
	if hit && cached.ownerUID == round.current.UID && cached.metadata.UID == meta.UID &&
		cached.metadata.ResourceVersion == meta.ResourceVersion {
		round.graph = meta
		result, err := c.clearGraphConditions(ctx, round)
		return cached.dcg, true, result, err
	}
	graph, err := c.client.GetBuildGraph(ctx, round.current.Namespace, round.current.Name, false)
	if errors.Is(err, ErrNotFound) {
		result, writeErr := c.graphUnavailable(ctx, round, "BuildGraphMissing")
		return nil, true, result, writeErr
	}
	if err != nil {
		return nil, true, controller.ReconcileResult{}, graphReadError(err)
	}
	dcg, result, err := c.acceptGraph(ctx, round, graph)
	return dcg, true, result, err
}

func firstGraphState(info *ebsv1.BuildInfo) bool {
	if findCondition(info.Status.Conditions, conditionBuildGraphUnavailable) != nil {
		return false
	}
	if info.Status.Phase != ebsv1.BuildInfoPending || len(info.Status.SpecStatus.Build) != 0 ||
		len(info.Status.SpecRepoNames) != 0 || len(info.Status.PendingJobCreates) != 0 {
		return false
	}
	for _, count := range info.Status.SpecStatus.DispatchCount {
		if count != 0 {
			return false
		}
	}
	return true
}

func graphIdentityReason(graph *ebsv1.BuildGraph, info *ebsv1.BuildInfo) string {
	if graph == nil || graph.Name != info.Name || graph.Namespace != info.Namespace ||
		graph.UID == "" || graph.ResourceVersion == "" {
		return "BuildGraphInvalid"
	}
	owner := metav1.GetControllerOf(graph)
	if owner == nil || owner.APIVersion != "ebs/v1" || owner.Kind != "BuildInfo" ||
		owner.Name != info.Name || owner.UID != info.UID {
		return "BuildGraphOwnerMismatch"
	}
	if graph.DeletionTimestamp != nil {
		return "BuildGraphDeleting"
	}
	return ""
}

func graphNodesValid(nodes map[string]ebsv1.DcgNodeState) bool {
	if nodes == nil {
		return false
	}
	out := make(map[string]map[string]int, len(nodes))
	for name := range nodes {
		if name == "" {
			return false
		}
		out[name] = map[string]int{}
	}
	for name, node := range nodes {
		for _, edges := range []map[string]ebsv1.VersionConstraint{node.InDep, node.InstallInDep} {
			for upstream := range edges {
				if out[upstream] == nil {
					return false
				}
				out[upstream][name]++
			}
		}
	}
	for name, node := range nodes {
		for _, downstream := range node.OutDep {
			if out[name][downstream] == 0 {
				return false
			}
			out[name][downstream]--
		}
		for _, count := range out[name] {
			if count != 0 {
				return false
			}
		}
	}
	return true
}

func (c *Controller) acceptGraph(ctx context.Context, round *reconcileRound, graph *ebsv1.BuildGraph) (*DcgDict, controller.ReconcileResult, error) {
	reason := graphIdentityReason(graph, round.current)
	if reason == "" && !graphNodesValid(graph.Status.Nodes) {
		reason = "BuildGraphInvalid"
	}
	if reason != "" {
		result, err := c.graphUnavailable(ctx, round, reason)
		return nil, result, err
	}
	dcg := DcgDictFromState(graph.Status.Nodes)
	// Cache only one graph representation; round.graph holds the optimistic-lock metadata, not a second nodes map.
	round.graph = &ebsv1.BuildGraph{ObjectMeta: *graph.ObjectMeta.DeepCopy(), TypeMeta: graph.TypeMeta}
	c.dcgDict.Set(round.key, &graphCacheEntry{
		ownerUID: round.current.UID, metadata: *graph.ObjectMeta.DeepCopy(), dcg: dcg,
	})
	result, err := c.clearGraphConditions(ctx, round)
	return dcg, result, err
}

func (c *Controller) graphUnavailable(ctx context.Context, round *reconcileRound, reason string) (controller.ReconcileResult, error) {
	c.dcgDict.Invalidate(round.key)
	next := round.current.DeepCopy()
	upsertCondition(&next.Status.Conditions, conditionBuildGraphUnavailable, reason, "BuildGraph "+round.key+" is unavailable")
	return c.writeStatusIfChanged(ctx, round, next)
}

func (c *Controller) clearGraphConditions(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	removeCondition(&next.Status.Conditions, conditionBuildGraphUnavailable)
	removeCondition(&next.Status.Conditions, ConditionDcgBuildFailed)
	return c.writeStatusIfChanged(ctx, round, next)
}

func graphWriteMatches(graph, intent *ebsv1.BuildGraph) bool {
	if graph == nil || graph.UID == "" || graph.ResourceVersion == "" || graph.Name != intent.Name ||
		graph.Namespace != intent.Namespace || graph.DeletionTimestamp != nil {
		return false
	}
	if intent.UID != "" && graph.UID != intent.UID {
		return false
	}
	owner, expected := metav1.GetControllerOf(graph), metav1.GetControllerOf(intent)
	return owner != nil && expected != nil && owner.APIVersion == expected.APIVersion &&
		owner.Kind == expected.Kind && owner.Name == expected.Name && owner.UID == expected.UID &&
		graphNodesValid(graph.Status.Nodes) && dcgStateMatches(graph.Status.Nodes, intent.Status.Nodes)
}

// writeGraph never retries an old candidate with a new resourceVersion.
func (c *Controller) writeGraph(ctx context.Context, round *reconcileRound, candidate *DcgDict) (*DcgDict, controller.ReconcileResult, error) {
	intent := newBuildGraph(round.current, candidate.ToState())
	creating := round.graph == nil
	if !creating {
		intent.ObjectMeta = *round.graph.ObjectMeta.DeepCopy()
	}
	var graph *ebsv1.BuildGraph
	var err error
	if creating {
		graph, err = c.client.CreateBuildGraph(ctx, intent.Namespace, intent.DeepCopy())
	} else {
		graph, err = c.client.UpdateBuildGraphStatus(ctx, intent.DeepCopy())
	}
	if err == nil && graphWriteMatches(graph, intent) {
		return c.acceptGraph(ctx, round, graph)
	}
	if err == nil {
		err = unknownWrite("write-graph", source.BuildGraphsGVR, fmt.Errorf("invalid BuildGraph response"))
	}
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return nil, controller.ReconcileResult{}, err
	}
	if writeErr.Outcome == clientpkg.WriteNotSent {
		var networkError net.Error
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) {
			return nil, controller.ReconcileResult{}, err
		}
		return nil, controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
	if writeErr.Outcome == clientpkg.WriteRejected {
		if creating && writeErr.StatusCode == 409 && apierrors.IsAlreadyExists(err) {
			graph, err = c.client.GetBuildGraph(ctx, intent.Namespace, intent.Name, false)
			if errors.Is(err, ErrNotFound) {
				return nil, graphRequeue(), nil
			}
			if err != nil {
				return nil, controller.ReconcileResult{}, graphReadError(err)
			}
			return c.acceptGraph(ctx, round, graph)
		}
		switch writeErr.StatusCode {
		case 404, 409:
			c.dcgDict.Invalidate(round.key)
			return nil, graphRequeue(), nil
		case 400, 401, 403, 422:
			return nil, controller.ReconcileResult{}, controller.NewPermanentError(err)
		default:
			return nil, controller.ReconcileResult{}, err
		}
	}
	if ctx.Err() != nil {
		return nil, controller.ReconcileResult{}, ctx.Err()
	}
	graph, err = c.client.GetBuildGraph(ctx, intent.Namespace, intent.Name, false)
	if errors.Is(err, ErrNotFound) {
		c.dcgDict.Invalidate(round.key)
		return nil, graphRequeue(), nil
	}
	if err != nil {
		return nil, controller.ReconcileResult{}, graphReadError(err)
	}
	if reason := graphIdentityReason(graph, round.current); reason != "" {
		result, err := c.graphUnavailable(ctx, round, reason)
		return nil, result, err
	}
	if !graphWriteMatches(graph, intent) {
		c.dcgDict.Invalidate(round.key)
		return nil, graphRequeue(), nil
	}
	return c.acceptGraph(ctx, round, graph)
}

func graphRequeue() controller.ReconcileResult {
	return controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}
}

func graphReadError(err error) error {
	var contract contractError
	var status apierrors.APIStatus
	if errors.As(err, &contract) {
		return controller.NewPermanentError(err)
	}
	if errors.As(err, &status) {
		switch status.Status().Code {
		case 400, 401, 403, 422:
			return controller.NewPermanentError(err)
		}
	}
	return err
}
