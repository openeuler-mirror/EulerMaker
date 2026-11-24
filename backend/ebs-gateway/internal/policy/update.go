package policy

import (
	"context"
	"maps"
	"net/http"
	"reflect"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/iam"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

var protectedMetadata = []string{
	"uid", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds",
	"generation", "managedFields", "finalizers", "ownerReferences",
}

func (a *Authorizer) ValidateUpdate(ctx context.Context, who identity.Principal, route Route, old, candidate mutation.Object) error {
	if err := validateUpdateIdentity(route, old, candidate); err != nil {
		return err
	}
	if route.Subresource == "status" {
		return validateStatusUpdate(who, route, old, candidate)
	}
	if err := validateOrdinaryMetadata(route, old, candidate); err != nil {
		return err
	}
	switch route.Resource {
	case "projects":
		return a.validateProjectUpdate(ctx, who, old, candidate)
	case "runners":
		return validateRunnerUpdate(who, old, candidate)
	case "users":
		return validateAdminUser(old, candidate)
	case "machineaccounts":
		return validateMachineAccountUpdate(who, old, candidate)
	default:
		return nil
	}
}

func validateUpdateIdentity(route Route, old, candidate mutation.Object) error {
	if route.Name == "" {
		return &Error{Status: http.StatusBadRequest, Message: "object name required"}
	}
	if !reflect.DeepEqual(old["apiVersion"], candidate["apiVersion"]) || !reflect.DeepEqual(old["kind"], candidate["kind"]) {
		return deny("resource identity cannot change")
	}
	for key, value := range old {
		if key != "metadata" && key != "spec" && key != "status" && !reflect.DeepEqual(value, candidate[key]) {
			return deny("protected object field changed")
		}
	}
	for key := range candidate {
		if key != "metadata" && key != "spec" && key != "status" {
			if _, exists := old[key]; !exists {
				return deny("protected object field added")
			}
		}
	}
	oldMeta := objectMap(old["metadata"])
	newMeta := objectMap(candidate["metadata"])
	if oldMeta == nil || newMeta == nil || newMeta["name"] != route.Name {
		return &Error{Status: http.StatusBadRequest, Message: "object name mismatch"}
	}
	if route.Project != "" && newMeta["namespace"] != route.Project {
		return &Error{Status: http.StatusBadRequest, Message: "object Project mismatch"}
	}
	if !reflect.DeepEqual(oldMeta["name"], newMeta["name"]) || !reflect.DeepEqual(oldMeta["namespace"], newMeta["namespace"]) {
		return deny("resource identity cannot change")
	}
	if route.Resource == "machineaccounts" && (!reflect.DeepEqual(oldMeta["uid"], newMeta["uid"]) || !reflect.DeepEqual(oldMeta["resourceVersion"], newMeta["resourceVersion"])) {
		return &Error{Status: http.StatusConflict, Message: "machine account changed; reload before updating grants"}
	}
	for _, name := range protectedMetadata {
		if !reflect.DeepEqual(oldMeta[name], newMeta[name]) {
			return deny("server metadata cannot change")
		}
	}
	if route.Resource != "runners" && !reflect.DeepEqual(oldMeta["resourceVersion"], newMeta["resourceVersion"]) {
		return deny("resourceVersion cannot be changed by Gateway caller")
	}
	return nil
}

func validateOrdinaryMetadata(route Route, old, candidate mutation.Object) error {
	// The apiserver preserves stored status on ordinary updates, including
	// when callers omit status or send a stale copy.
	oldMeta := objectMap(old["metadata"])
	newMeta := objectMap(candidate["metadata"])
	if !equalMetadataExceptUserFields(oldMeta, newMeta, route.Resource == "runners") {
		return deny("protected metadata changed")
	}
	return nil
}

func (a *Authorizer) validateProjectUpdate(ctx context.Context, who identity.Principal, old, candidate mutation.Object) error {
	oldMeta := objectMap(old["metadata"])
	newMeta := objectMap(candidate["metadata"])
	if who.Scope == identity.AdminScope {
		// Admin owners may manage purpose, without gaining ordinary Project update privileges.
		oldLabels := maps.Clone(objectMap(oldMeta["labels"]))
		newLabels := maps.Clone(objectMap(newMeta["labels"]))
		delete(oldLabels, ebsv1.BuildPurposeLabel)
		delete(newLabels, ebsv1.BuildPurposeLabel)
		if !reflect.DeepEqual(old["spec"], candidate["spec"]) ||
			!reflect.DeepEqual(oldMeta["annotations"], newMeta["annotations"]) ||
			!maps.EqualFunc(oldLabels, newLabels, reflect.DeepEqual) {
			return deny("Admin Project updates may only change build purpose")
		}
	}
	return a.validateProjectLabels(ctx, who, oldMeta, newMeta)
}

func validateRunnerUpdate(who identity.Principal, old, candidate mutation.Object) error {
	if who.Type != identity.RunnerType {
		return nil
	}
	return validateRunnerOrdinary(objectMap(old["metadata"]), objectMap(candidate["metadata"]), objectMap(old["spec"]), objectMap(candidate["spec"]))
}

func validateMachineAccountUpdate(who identity.Principal, old, candidate mutation.Object) error {
	if who.Type != identity.UserType || who.Scope != identity.AdminScope {
		return deny("machine account updates require Admin")
	}
	oldSpec, newSpec := maps.Clone(objectMap(old["spec"])), maps.Clone(objectMap(candidate["spec"]))
	if (oldSpec["type"] != "service" && oldSpec["type"] != "runner") || newSpec["type"] != oldSpec["type"] {
		return deny("machine account type is immutable")
	}
	allowed := []string{"tokenTTLSeconds"}
	if oldSpec["type"] == "service" {
		allowed = append(allowed, "projects", "permissions")
	}
	for _, key := range allowed {
		delete(oldSpec, key)
		delete(newSpec, key)
	}
	if !reflect.DeepEqual(oldSpec, newSpec) || !reflect.DeepEqual(old["metadata"], candidate["metadata"]) || !reflect.DeepEqual(old["status"], candidate["status"]) {
		return deny("only token lifetime and service account grants can be updated")
	}
	return nil
}

func objectMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func equalMetadataExceptVersion(old, next map[string]any) bool {
	for key, value := range old {
		if key != "resourceVersion" && !reflect.DeepEqual(value, next[key]) {
			return false
		}
	}
	for key := range next {
		if key != "resourceVersion" {
			if _, exists := old[key]; !exists {
				return false
			}
		}
	}
	return true
}

func equalMetadataExceptUserFields(old, next map[string]any, allowVersion bool) bool {
	for key, value := range old {
		if key != "labels" && key != "annotations" && !(allowVersion && key == "resourceVersion") && !reflect.DeepEqual(value, next[key]) {
			return false
		}
	}
	for key := range next {
		if key != "labels" && key != "annotations" && !(allowVersion && key == "resourceVersion") {
			if _, exists := old[key]; !exists {
				return false
			}
		}
	}
	return true
}

func validateStatusUpdate(who identity.Principal, route Route, old, candidate mutation.Object) error {
	metadataEqual := reflect.DeepEqual(old["metadata"], candidate["metadata"])
	if route.Resource == "runners" {
		metadataEqual = metadataEqual || equalMetadataExceptVersion(objectMap(old["metadata"]), objectMap(candidate["metadata"]))
	}
	if !reflect.DeepEqual(old["spec"], candidate["spec"]) || !metadataEqual {
		return deny("status update cannot modify metadata or spec")
	}
	if route.Resource == "jobs" && who.Type != identity.RunnerType {
		return deny("Job status is not user-writable")
	}
	if who.Type != identity.RunnerType {
		return nil
	}
	oldStatus := objectMap(old["status"])
	newStatus := objectMap(candidate["status"])
	var allowed map[string]bool
	switch route.Resource {
	case "runners":
		allowed = map[string]bool{"phase": true, "conditions": true, "capacity": true, "allocatable": true, "activeJobs": true, "addresses": true, "info": true, "heartbeat": true}
	case "jobs":
		if oldStatus["runner"] != who.Runner || newStatus["runner"] != who.Runner {
			return deny("Job is not assigned to Runner")
		}
		allowed = map[string]bool{"phase": true, "stage": true, "result": true, "startTime": true, "endTime": true, "message": true}
	default:
		return deny("Runner cannot update this status")
	}
	for key := range oldStatus {
		if !allowed[key] && !reflect.DeepEqual(oldStatus[key], newStatus[key]) {
			return deny("protected status field changed")
		}
	}
	for key := range newStatus {
		if !allowed[key] {
			if _, exists := oldStatus[key]; !exists {
				return deny("protected status field added")
			}
		}
	}
	return nil
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

func (a *Authorizer) validateProjectLabels(ctx context.Context, who identity.Principal, oldMeta, newMeta map[string]any) error {
	oldLabels := objectMap(oldMeta["labels"])
	newLabels := objectMap(newMeta["labels"])
	oldPurpose, oldSet := oldLabels[ebsv1.BuildPurposeLabel]
	newPurpose, newSet := newLabels[ebsv1.BuildPurposeLabel]
	if (oldSet != newSet || !reflect.DeepEqual(oldPurpose, newPurpose)) &&
		(!who.IsPrivileged() || oldLabels[ebsv1.ProjectOwnerUserLabel] != who.Subject) {
		return deny("Project build purpose requires an Ops or Admin owner")
	}
	if who.Scope == identity.UserScope && !reflect.DeepEqual(oldLabels[ebsv1.ProjectTypeLabel], newLabels[ebsv1.ProjectTypeLabel]) {
		if oldLabels[ebsv1.ProjectTypeLabel] != nil || newLabels[ebsv1.ProjectTypeLabel] != "personal" {
			return deny("ordinary users cannot change Project type")
		}
	}
	oldOwner := oldLabels[ebsv1.ProjectOwnerUserLabel]
	newOwner := newLabels[ebsv1.ProjectOwnerUserLabel]
	if !reflect.DeepEqual(oldOwner, newOwner) {
		return deny("Project owner cannot be changed")
	}
	for name, value := range newLabels {
		if !strings.HasPrefix(name, ebsv1.ProjectMemberUserLabelPrefix) || reflect.DeepEqual(oldLabels[name], value) {
			continue
		}
		if value != "true" {
			return deny("invalid Project member label")
		}
		if err := a.validateMember(ctx, strings.TrimPrefix(name, ebsv1.ProjectMemberUserLabelPrefix)); err != nil {
			return err
		}
	}
	return nil
}

func (a *Authorizer) validateMember(ctx context.Context, name string) error {
	client := iam.New(a.upstream)
	user, status, err := client.GetUser(ctx, name)
	if err != nil || status != http.StatusOK || user.Name != name || !user.Enabled || len(user.Scopes) != 1 ||
		(user.Scopes[0] != identity.UserScope && user.Scopes[0] != identity.OpsScope) {
		return deny("Project user must exist, be enabled and have User or Ops scope")
	}
	return nil
}

func validateAdminUser(old, candidate mutation.Object) error {
	oldSpec := objectMap(old["spec"])
	newSpec := objectMap(candidate["spec"])
	if hasAdminScope(oldSpec["scopes"]) || hasAdminScope(newSpec["scopes"]) {
		return deny("Admin User cannot be managed through Gateway")
	}
	for key, value := range oldSpec {
		if !adminUserFieldWritable(key) && !reflect.DeepEqual(value, newSpec[key]) {
			return deny("protected User field changed")
		}
	}
	for key := range newSpec {
		if !adminUserFieldWritable(key) {
			if _, exists := oldSpec[key]; !exists {
				return deny("protected User field added")
			}
		}
	}
	return nil
}

func hasAdminScope(raw any) bool {
	values, ok := raw.([]any)
	return ok && len(values) == 1 && values[0] == string(identity.AdminScope)
}

func adminUserFieldWritable(key string) bool {
	switch key {
	case "enabled", "scopes", "displayName", "email":
		return true
	default:
		return false
	}
}
