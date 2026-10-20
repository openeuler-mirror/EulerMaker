package validation

import (
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateBuildGraph(obj *ebsv1.BuildGraph) field.ErrorList {
	var errs field.ErrorList
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.APIVersion != "ebs/v1" || owner.Kind != "BuildInfo" || owner.Name != obj.Name || owner.UID == "" {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "ownerReferences"), obj.OwnerReferences,
			"requires the same-name BuildInfo controller owner with UID"))
	}
	nodes := obj.Status.Nodes
	path := field.NewPath("status", "nodes")
	if nodes == nil {
		return append(errs, field.Required(path, "must be an object; an empty graph is allowed"))
	}
	out := make(map[string]map[string]int, len(nodes))
	for name := range nodes {
		out[name] = map[string]int{}
	}
	for name, node := range nodes {
		if name == "" {
			errs = append(errs, field.Invalid(path, name, "node name must not be empty"))
		}
		for edgeKind, edges := range map[string]map[string]ebsv1.VersionConst{"inDep": node.InDep, "installInDep": node.InstallInDep} {
			for upstream := range edges {
				if _, exists := nodes[upstream]; !exists {
					errs = append(errs, field.Invalid(path.Key(name).Child(edgeKind).Key(upstream), upstream, "unknown node"))
				} else {
					out[upstream][name]++
				}
			}
		}
	}
	for name, node := range nodes {
		actual := map[string]int{}
		for _, downstream := range node.OutDep {
			actual[downstream]++
		}
		if !reflect.DeepEqual(actual, out[name]) {
			errs = append(errs, field.Invalid(path.Key(name).Child("outDep"), node.OutDep,
				"must match reverse build/install edges with the same multiplicity"))
		}
	}
	return errs
}

func ValidateBuildGraphUpdate(obj, old *ebsv1.BuildGraph) field.ErrorList {
	errs := ValidateBuildGraph(obj)
	if !reflect.DeepEqual(obj.OwnerReferences, old.OwnerReferences) {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "ownerReferences"), "owner is immutable"))
	}
	return errs
}

func ValidateBuildGraphStatusUpdate(obj, old *ebsv1.BuildGraph) field.ErrorList {
	errs := ValidateBuildGraphUpdate(obj, old)
	path := field.NewPath("status", "nodes")
	if len(obj.Status.Nodes) != len(old.Status.Nodes) {
		errs = append(errs, field.Forbidden(path, "node set is immutable"))
	}
	for name, prev := range old.Status.Nodes {
		next, exists := obj.Status.Nodes[name]
		if !exists || prev.Version != next.Version || !sameBuildEdges(prev.InDep, next.InDep) || (prev.BootstrapBreak && !next.BootstrapBreak) {
			errs = append(errs, field.Forbidden(path.Key(name), "nodes, versions, build edges and existing breakpoints are immutable"))
		}
		for upstream, constraint := range prev.InstallInDep {
			if value, ok := next.InstallInDep[upstream]; !ok || value != constraint {
				errs = append(errs, field.Forbidden(path.Key(name).Child("installInDep"), "existing install edges are immutable"))
			}
		}
	}
	return errs
}

func sameBuildEdges(a, b map[string]ebsv1.VersionConst) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}
