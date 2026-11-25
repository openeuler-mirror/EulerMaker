package policy

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
	iamv1 "ebs-api/iam/v1"
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

func deny(message string) error {
	return &Error{Status: http.StatusForbidden, Message: message}
}

type Authorizer struct {
	upstream *upstream.Client
}

func New(upstreamClient *upstream.Client) *Authorizer {
	return &Authorizer{upstream: upstreamClient}
}

func (a *Authorizer) Authorize(ctx context.Context, who identity.Principal, route Route) error {
	if who.Type == identity.ServiceType {
		return authorizeService(who, route)
	}
	switch route.Resource {
	case "runners":
		return authorizeRunner(who, route)
	case "projects":
		return a.authorizeProject(ctx, who, route)
	case "builds":
		return a.authorizeBuild(ctx, who, route)
	case "jobs":
		return a.authorizeJob(ctx, who, route)
	case "snapshots", "buildinfos", "buildgraphs", "rpmrepos", "oldjobs":
		return a.authorizeReadOnlyResource(ctx, who, route)
	default:
		return deny("resource is not exposed")
	}
}

func (a *Authorizer) authorizeBuild(ctx context.Context, who identity.Principal, route Route) error {
	if who.Type == identity.RunnerType {
		return deny("Runner project access denied")
	}
	if route.Method == http.MethodDelete {
		if who.Type != identity.UserType || who.Subject == "" || route.Project == "" {
			return deny("only the Project owner can delete Build")
		}
		access, err := a.projectAccess(ctx, route.Project, who.Subject)
		if err != nil {
			return err
		}
		if !access.owner {
			return deny("only the Project owner can delete Build")
		}
		return nil
	}
	if who.Type == identity.UserType && who.Scope == identity.AdminScope {
		return nil
	}
	_, err := a.projectMembership(ctx, who, route.Project)
	return err
}

func (a *Authorizer) authorizeJob(ctx context.Context, who identity.Principal, route Route) error {
	if route.Subresource == "abort" {
		return a.jobAbort(ctx, who, route)
	}
	if who.Type == identity.RunnerType {
		if route.Project != "" && route.Name != "" &&
			((route.Subresource == "" && route.Method == http.MethodGet) ||
				(route.Subresource == "status" && (route.Method == http.MethodPut || route.Method == http.MethodPatch))) {
			return nil // Object assignment is verified by the Job handler before forwarding.
		}
		return deny("Runner project access denied")
	}
	if who.Type == identity.UserType && who.Scope == identity.AdminScope {
		if route.Subresource == "status" {
			return deny("Admin cannot update Job status")
		}
		return nil
	}
	access, err := a.projectMembership(ctx, who, route.Project)
	if err != nil {
		return err
	}
	if route.Subresource == "status" && isWrite(route.Method) {
		return deny("user cannot update Job status")
	}
	if !access.owner && route.Method == http.MethodDelete {
		return deny("Project member cannot delete resource")
	}
	return nil
}

func (a *Authorizer) authorizeReadOnlyResource(ctx context.Context, who identity.Principal, route Route) error {
	if route.Method != http.MethodGet {
		return deny("resource is read-only through Gateway")
	}
	if who.Type == identity.UserType && who.Scope == identity.AdminScope {
		return nil
	}
	_, err := a.projectMembership(ctx, who, route.Project)
	return err
}

func authorizeService(who identity.Principal, route Route) error {
	project, permission := ServiceOperation(route)
	if permission == "" {
		return deny("service operation is not allowed")
	}
	grants := who.ServiceGrants
	if grants == nil || !slices.Contains(grants.Projects, project) || !slices.Contains(grants.Permissions, permission) {
		return deny("service account lacks project or operation permission")
	}
	return nil
}

// ServiceOperation identifies the project and permission required by an exposed service write route.
func ServiceOperation(route Route) (string, iamv1.MachineAccountPermission) {
	switch route.Resource {
	case "projects":
		if route.Project == "" && route.Name != "" && route.Subresource == "" &&
			(route.Method == http.MethodPut || route.Method == http.MethodPatch) {
			return route.Name, iamv1.MachinePermissionProjectsUpdate
		}
	case "builds":
		if route.Project != "" && route.Method == http.MethodPost {
			if route.Name == "" && route.Subresource == "" {
				return route.Project, iamv1.MachinePermissionBuildsCreate
			}
			if route.Name != "" && route.Subresource == "abort" {
				return route.Project, iamv1.MachinePermissionBuildsAbort
			}
		}
	}
	return "", ""
}

func (a *Authorizer) jobAbort(ctx context.Context, who identity.Principal, route Route) error {
	if route.Method != http.MethodPost || route.Project == "" || route.Name == "" {
		return deny("Job abort requires a named Job and POST")
	}
	if !who.IsUser() {
		return deny("Job abort requires a project member")
	}
	access, err := a.projectAccess(ctx, route.Project, who.Subject)
	if err != nil {
		return err
	}
	if !access.owner && !access.member {
		return deny("Job abort requires a project member")
	}
	return nil
}

// AuthorizeConfig restricts Config reads and writes to Ops/Admin.
func AuthorizeConfig(_ context.Context, who identity.Principal, route Route) error {
	if who.Type == identity.ServiceType {
		return authorizeService(who, route)
	}
	if !who.IsPrivileged() {
		return deny("Config access requires Ops")
	}
	if route.Subresource != "" {
		return deny("Config subresource is not exposed")
	}
	if route.Method == http.MethodDelete && isBuiltInConfig(route.Name) {
		return deny("built-in Config cannot be deleted")
	}
	return nil
}

func isBuiltInConfig(name string) bool {
	switch name {
	case ebsv1.BuildTargetConfigName, ebsv1.BuildResourceConfigName, ebsv1.ServiceConfigName:
		return true
	default:
		return false
	}
}

// AuthorizeScript checks Script reads and writes for the authenticated caller.
func AuthorizeScript(_ context.Context, who identity.Principal, route Route) error {
	if who.Type == identity.ServiceType {
		return authorizeService(who, route)
	}
	if route.Subresource != "" {
		return deny("Script subresource is not exposed")
	}
	if who.Type == identity.RunnerType {
		if route.Name != "" && route.Method == http.MethodGet {
			return nil
		}
		return deny("Runner can only read a named Script")
	}
	if route.Method == http.MethodGet {
		return nil
	}
	if !who.IsPrivileged() {
		return deny("Script modification requires Ops")
	}
	return nil
}

func isWrite(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func IsPublicRead(route Route) bool {
	if route.Resource == "oldjobs" && route.Subresource != "" {
		return false
	}
	if route.Method != http.MethodGet {
		return false
	}
	if route.Resource == "build-targets" {
		return route.Project == "" && route.Name == "" && route.Subresource == ""
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
	case "snapshots", "builds", "buildinfos", "buildgraphs", "rpmrepos", "jobs", "oldjobs":
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
