package policy

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

type access struct {
	owner  bool
	member bool
}

func (a *Authorizer) AuthorizeProjectOwner(ctx context.Context, who identity.Principal, project string) error {
	if !who.IsUser() || who.Subject == "" || project == "" {
		return deny("only the Project owner can perform this operation")
	}
	access, _, err := a.projectAccess(ctx, project, who.Subject, false)
	if err != nil {
		return err
	}
	if !access.owner {
		return deny("only the Project owner can perform this operation")
	}
	return nil
}

// AuthorizeProjectMember requires a user to be the Project owner or a member, including privileged users.
func (a *Authorizer) AuthorizeProjectMember(ctx context.Context, who identity.Principal, project string) error {
	if who.Type == identity.RunnerType {
		return deny("Runner project access denied")
	}
	if !who.IsUser() {
		return deny("project access denied")
	}
	if project == "" {
		return deny("project name required")
	}
	membership, _, err := a.projectAccess(ctx, project, who.Subject, false)
	if err != nil {
		return err
	}
	if !membership.owner && !membership.member {
		return deny("project access denied")
	}
	return nil
}

func (a *Authorizer) authorizeProjectUpdate(ctx context.Context, who identity.Principal, project string) ([]byte, error) {
	if !who.IsUser() || who.Subject == "" || project == "" {
		return nil, deny("only the Project owner can perform this operation")
	}
	access, raw, err := a.projectAccess(ctx, project, who.Subject, true)
	if err != nil {
		return nil, err
	}
	if !access.owner {
		return nil, deny("only the Project owner can perform this operation")
	}
	return raw, nil
}

func (a *Authorizer) projectAccess(ctx context.Context, project, subject string, forUpdate bool) (access, []byte, error) {
	path := "/apis/ebs/v1/projects/" + url.PathEscape(project)
	limit := int64(1 << 20)
	if forUpdate {
		limit = a.bodyLimit
	} else {
		path += "?includeFields=metadata.name,metadata.labels"
	}
	response, err := a.upstream.Do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return access{}, nil, &Error{Status: http.StatusServiceUnavailable, Message: "Project lookup unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusNotFound {
			return access{}, nil, &Error{Status: http.StatusNotFound, Message: "Project not found"}
		}
		return access{}, nil, &Error{Status: http.StatusServiceUnavailable, Message: "Project lookup unavailable"}
	}
	var object struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(raw)) > limit || json.Unmarshal(raw, &object) != nil {
		return access{}, nil, &Error{Status: http.StatusBadGateway, Message: "invalid Project response"}
	}
	if object.Metadata.Name != project {
		return access{}, nil, &Error{Status: http.StatusBadGateway, Message: "Project identity mismatch"}
	}
	return access{
		owner:  object.Metadata.Labels[ebsv1.ProjectOwnerUserLabel] == subject,
		member: object.Metadata.Labels[ebsv1.ProjectMemberUserLabelPrefix+subject] == "true",
	}, raw, nil
}

func (a *Authorizer) prepareProjectCreate(ctx context.Context, who identity.Principal, object mutation.Object) error {
	if who.Type == identity.RunnerType {
		return deny("Runner cannot create Project")
	}
	meta := objectMap(object["metadata"])
	if meta == nil {
		return &Error{Status: http.StatusBadRequest, Message: "Project metadata is required"}
	}
	labels := objectMap(meta["labels"])
	if labels == nil {
		labels = make(map[string]any)
	}
	if _, exists := labels[ebsv1.BuildPurposeLabel]; exists && !who.IsPrivileged() {
		return deny("Project build purpose requires Ops or Admin")
	}
	labels[ebsv1.ProjectOwnerUserLabel] = who.Subject
	if who.Scope == identity.UserScope {
		if value, exists := labels[ebsv1.ProjectTypeLabel]; exists && value != "personal" {
			return deny("ordinary users can only create personal Projects")
		}
	}
	for key, value := range labels {
		if !strings.HasPrefix(key, ebsv1.ProjectMemberUserLabelPrefix) {
			continue
		}
		if value != "true" || a.validateMember(ctx, strings.TrimPrefix(key, ebsv1.ProjectMemberUserLabelPrefix)) != nil {
			return deny("invalid Project member")
		}
	}
	meta["labels"] = labels
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
	client := iamclient.New(a.upstream)
	user, status, err := client.GetUser(ctx, name)
	if err != nil || status != http.StatusOK || user.Name != name || user.Spec.Enabled == nil || !*user.Spec.Enabled || len(user.Spec.Scopes) != 1 ||
		(identity.Scope(user.Spec.Scopes[0]) != identity.UserScope && identity.Scope(user.Spec.Scopes[0]) != identity.OpsScope) {
		return deny("Project user must exist, be enabled and have User or Ops scope")
	}
	return nil
}
