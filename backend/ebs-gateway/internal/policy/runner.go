package policy

import (
	"reflect"
	"strings"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

func validateRunnerCreate(object mutation.Object, runner string) error {
	meta := objectMap(object["metadata"])
	if meta == nil || meta["name"] != runner {
		return deny("Runner name must match token")
	}
	for key := range meta {
		if key != "name" && key != "labels" {
			return deny("protected Runner metadata on create")
		}
	}
	for name := range objectMap(meta["labels"]) {
		if !runnerLabelWritable(name) {
			return deny("unsupported Runner label")
		}
	}
	for key := range objectMap(object["spec"]) {
		if key != "instanceID" && key != "type" && key != "arch" && key != "taints" {
			return deny("protected Runner spec on create")
		}
	}
	if value, exists := object["status"]; exists {
		status, ok := value.(map[string]any)
		if !ok || len(status) != 0 {
			return deny("Runner status must be empty on create")
		}
	}
	return nil
}

func validateRunnerUpdate(who identity.Principal, old, candidate mutation.Object) error {
	if who.Type != identity.RunnerType {
		return nil
	}
	return validateRunnerOrdinary(objectMap(old["metadata"]), objectMap(candidate["metadata"]), objectMap(old["spec"]), objectMap(candidate["spec"]))
}

func validateRunnerOrdinary(oldMeta, newMeta, oldSpec, newSpec map[string]any) error {
	if oldSpec == nil || newSpec == nil {
		return deny("Runner spec cannot be removed")
	}
	for key := range oldSpec {
		if key != "type" && key != "arch" && !reflect.DeepEqual(oldSpec[key], newSpec[key]) {
			return deny("protected Runner spec changed")
		}
	}
	for key := range newSpec {
		if key != "type" && key != "arch" {
			if _, exists := oldSpec[key]; !exists {
				return deny("protected Runner spec added")
			}
		}
	}
	if !reflect.DeepEqual(oldMeta["annotations"], newMeta["annotations"]) {
		return deny("Runner annotations cannot change")
	}
	oldLabels := objectMap(oldMeta["labels"])
	newLabels := objectMap(newMeta["labels"])
	for key, oldValue := range oldLabels {
		if !runnerLabelWritable(key) && !reflect.DeepEqual(oldValue, newLabels[key]) {
			return deny("protected Runner label changed")
		}
	}
	for key := range newLabels {
		if !runnerLabelWritable(key) {
			if _, exists := oldLabels[key]; !exists {
				return deny("protected Runner label added")
			}
		}
	}
	return nil
}

func runnerLabelWritable(name string) bool {
	return name == "ebs.io/runner-type" || name == "ebs.io/runner-arch" || strings.HasPrefix(name, "ebs.io/runner-capability.")
}
