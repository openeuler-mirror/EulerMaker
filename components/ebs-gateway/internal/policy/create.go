package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

func (a *Authorizer) PrepareCreate(ctx context.Context, who identity.Principal, route Route, raw []byte) ([]byte, error) {
	object, err := mutation.ParseObject(raw)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid JSON object"}
	}
	if route.Project != "" {
		if meta := objectMap(object["metadata"]); meta != nil {
			if namespace, present := meta["namespace"]; present && namespace != route.Project {
				return nil, &Error{Status: http.StatusBadRequest, Message: "object Project mismatch"}
			}
		}
	}
	switch route.Resource {
	case "projects":
		if who.Scope == identity.RunnerScope {
			return nil, deny("Runner cannot create Project")
		}
		meta := objectMap(object["metadata"])
		if meta == nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "Project metadata is required"}
		}
		labels := objectMap(meta["labels"])
		if labels == nil {
			labels = make(map[string]any)
		}
		if who.Scope == identity.UserScope || who.Scope == identity.OpsScope {
			labels["ebs.io/owner-user"] = who.Subject
		} else if who.Scope == identity.AdminScope || who.Scope == identity.SystemScope {
			owner, ok := labels["ebs.io/owner-user"].(string)
			if !ok || a.validateMember(ctx, owner) != nil {
				return nil, deny("Project owner must be an enabled User or Ops")
			}
		}
		if who.Scope == identity.UserScope {
			if value, exists := labels["project.ebs.io/type"]; exists && value != "personal" {
				return nil, deny("ordinary users can only create personal Projects")
			}
		}
		for key, value := range labels {
			if !strings.HasPrefix(key, "ebs.io/member-user.") {
				continue
			}
			if value != "true" || a.validateMember(ctx, strings.TrimPrefix(key, "ebs.io/member-user.")) != nil {
				return nil, deny("invalid Project member")
			}
		}
		meta["labels"] = labels
	case "runners":
		if who.Scope != identity.RunnerScope {
			break
		}
		if err := validateRunnerCreate(object, who.Runner); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode object: %w", err)
	}
	return encoded, nil
}

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
		if key != "instanceId" && key != "type" && key != "arch" {
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
