package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

type Route struct {
	Resource    string
	Project     string
	Name        string
	Subresource string
	Method      string
}

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func deny(message string) error { return &Error{Status: http.StatusForbidden, Message: message} }

type Authorizer struct{ upstream *upstream.Client }

func New(upstreamClient *upstream.Client) *Authorizer {
	return &Authorizer{upstream: upstreamClient}
}

func (a *Authorizer) Authorize(ctx context.Context, who identity.Principal, route Route) error {
	if IsReadOnlyProjectResource(route.Resource) && route.Method != http.MethodGet {
		return deny("resource is read-only through Gateway")
	}
	if route.Subresource == "abort" && route.Resource == "jobs" {
		return a.jobAbort(ctx, who, route)
	}
	if route.Resource == "builds" && (route.Method == http.MethodPut || route.Method == http.MethodPatch) {
		return deny("Build updates are not available through Gateway")
	}
	switch route.Resource {
	case "configs":
		return authorizeConfig(who, route)
	case "scripts":
		return authorizeScript(who, route)
	case "runners":
		return authorizeRunner(who, route)
	case "users", "machineaccounts":
		if who.Scope != identity.AdminScope {
			return deny("IAM administration requires Admin")
		}
		return nil
	case "projects", "snapshots", "builds", "buildinfos", "rpmrepos", "jobs":
		return a.projectResource(ctx, who, route)
	default:
		return deny("resource is not exposed")
	}
}

func IsReadOnlyProjectResource(resource string) bool {
	switch resource {
	case "snapshots", "buildinfos", "rpmrepos":
		return true
	default:
		return false
	}
}

func (a *Authorizer) jobAbort(ctx context.Context, who identity.Principal, route Route) error {
	if route.Method != http.MethodPost || route.Project == "" || route.Name == "" {
		return deny("Job abort requires a named Job and POST")
	}
	if !who.IsUser() {
		return deny("Job abort requires a project member")
	}
	access, err := a.projectAccess(ctx, route.Project, who.Subject)
	if err != nil || (!access.owner && !access.member) {
		return deny("Job abort requires a project member")
	}
	return nil
}

func authorizeConfig(who identity.Principal, route Route) error {
	if !who.IsPrivileged() {
		return deny("Config access requires Ops")
	}
	if route.Subresource != "" {
		return deny("Config subresource is not exposed")
	}
	if route.Method == http.MethodDelete && (route.Name == "build-target" || route.Name == "build-resource") {
		return deny("built-in Config cannot be deleted")
	}
	return nil
}

func authorizeScript(who identity.Principal, route Route) error {
	if route.Subresource != "" {
		return deny("Script subresource is not exposed")
	}
	if who.Type == identity.RunnerType {
		if route.Name != "" && (route.Method == http.MethodGet || route.Method == http.MethodHead) {
			return nil
		}
		return deny("Runner can only read a named Script")
	}
	if route.Method == http.MethodGet || route.Method == http.MethodHead {
		return nil
	}
	if !who.IsPrivileged() {
		return deny("Script modification requires Ops")
	}
	return nil
}

func authorizeRunner(who identity.Principal, route Route) error {
	if who.IsPrivileged() {
		return nil
	}
	if who.Type != identity.RunnerType || who.Runner == "" || who.Subject != who.Runner {
		return deny("Runner access denied")
	}
	if route.Name == "" {
		if route.Method == http.MethodPost && route.Subresource == "" {
			return nil
		}
		return deny("Runner collection access denied")
	}
	if route.Name != who.Runner {
		return deny("Runner identity mismatch")
	}
	switch route.Subresource {
	case "":
		if route.Method == http.MethodGet || route.Method == http.MethodHead || route.Method == http.MethodPut || route.Method == http.MethodPatch {
			return nil
		}
	case "status":
		if route.Method == http.MethodPut || route.Method == http.MethodPatch {
			return nil
		}
	case "jobs":
		if route.Method == http.MethodGet {
			return nil
		}
	}
	return deny("Runner operation denied")
}

func (a *Authorizer) projectResource(ctx context.Context, who identity.Principal, route Route) error {
	if who.Type == identity.RunnerType {
		if route.Resource == "jobs" && route.Project != "" && route.Name != "" &&
			((route.Subresource == "" && (route.Method == http.MethodGet || route.Method == http.MethodHead)) ||
				(route.Subresource == "status" && (route.Method == http.MethodPut || route.Method == http.MethodPatch))) {
			return nil // Object assignment is verified by the Job handler before forwarding.
		}
		return deny("Runner project access denied")
	}
	if route.Resource == "projects" && route.Name != "" && isWrite(route.Method) {
		if who.Scope != identity.UserScope && who.Scope != identity.OpsScope {
			return deny("only the Project owner can modify Project")
		}
		access, err := a.projectAccess(ctx, route.Name, who.Subject)
		if err != nil || !access.owner {
			return deny("only the Project owner can modify Project")
		}
		return nil
	}
	if who.Type == identity.UserType && who.Scope == identity.AdminScope {
		if route.Resource == "jobs" && route.Subresource == "status" && who.Scope == identity.AdminScope {
			return deny("Admin cannot update Job status")
		}
		return nil
	}
	if !who.IsUser() {
		return deny("project access denied")
	}
	if route.Resource == "projects" && route.Name == "" && route.Method == http.MethodPost {
		return nil // Creation injects the caller's owner label in the Project handler.
	}
	project := route.Project
	if route.Resource == "projects" {
		project = route.Name
	}
	if project == "" {
		return deny("project name required")
	}
	access, err := a.projectAccess(ctx, project, who.Subject)
	if err != nil {
		return deny("project access could not be confirmed")
	}
	if !access.owner && !access.member {
		return deny("project access denied")
	}
	if route.Resource == "projects" {
		if !access.owner && isWrite(route.Method) {
			return deny("only the Project owner can modify Project")
		}
		return nil
	}
	if route.Resource == "jobs" && route.Subresource == "status" && isWrite(route.Method) {
		return deny("user cannot update Job status")
	}
	if access.member && !access.owner && route.Method == http.MethodDelete {
		return deny("Project member cannot delete resource")
	}
	return nil
}

func isWrite(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

type access struct{ owner, member bool }

func (a *Authorizer) projectAccess(ctx context.Context, project, subject string) (access, error) {
	response, err := a.upstream.Do(ctx, http.MethodGet, "/apis/ebs/v1/projects/"+url.PathEscape(project), nil, nil)
	if err != nil {
		return access{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return access{}, fmt.Errorf("Project lookup returned %d", response.StatusCode)
	}
	var object struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&object); err != nil {
		return access{}, err
	}
	if object.Metadata.Name != project {
		return access{}, fmt.Errorf("Project identity mismatch")
	}
	return access{
		owner:  object.Metadata.Labels["ebs.io/owner-user"] == subject,
		member: object.Metadata.Labels["ebs.io/member-user."+subject] == "true",
	}, nil
}

func IsPublicRead(route Route) bool {
	if route.Method != http.MethodGet && route.Method != http.MethodHead {
		return false
	}
	if IsReadOnlyProjectResource(route.Resource) && route.Method != http.MethodGet {
		return false
	}
	if route.Subresource != "" && route.Subresource != "status" {
		return false
	}
	if route.Subresource == "status" && route.Name == "" {
		return false
	}
	if route.Resource == "projects" {
		return true
	}
	if route.Project == "" {
		return false
	}
	switch route.Resource {
	case "snapshots", "builds", "buildinfos", "rpmrepos", "jobs":
		return true
	default:
		return false
	}
}

func HasWatch(query url.Values) bool {
	for _, value := range query["watch"] {
		if !strings.EqualFold(value, "false") {
			return true
		}
	}
	return false
}
