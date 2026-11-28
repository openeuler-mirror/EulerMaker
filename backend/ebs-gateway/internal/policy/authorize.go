package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

type ObjectAccess string

const (
	ObjectAccessNone   ObjectAccess = ""
	ProjectOwner       ObjectAccess = "project-owner"
	ProjectOwnerUpdate ObjectAccess = "project-owner-update"
	ProjectMember      ObjectAccess = "project-member"
	RunnerSelf         ObjectAccess = "runner-self"
	AssignedJob        ObjectAccess = "assigned-job"
)

type RouteIdentities uint8

const (
	AllowAnonymous RouteIdentities = 1 << iota
	AllowUser
	AllowOps
	AllowAdmin
	AllowRunner
	AllowService

	AllowPrivileged    = AllowOps | AllowAdmin
	AllowUsers         = AllowUser | AllowOps | AllowAdmin
	AllowAuthenticated = AllowUsers | AllowRunner | AllowService
)

type Rule struct {
	Identities        RouteIdentities
	UserJWTOnly       bool
	ObjectAccess      ObjectAccess
	WatchAccess       ObjectAccess
	ServicePermission iamv1.MachineAccountPermission
}

type Request struct {
	Method  string
	Project string
	Runner  string
	Path    string
	Watch   bool
}

type Result struct {
	UpdateObject []byte
}

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
	upstream  *upstream.Client
	bodyLimit int64
}

func New(upstreamClient *upstream.Client, bodyLimit int64) *Authorizer {
	return &Authorizer{upstream: upstreamClient, bodyLimit: bodyLimit}
}

// ReadAssignedJob projects assignment fields for reads, retaining a full object only for status updates.
func (a *Authorizer) ReadAssignedJob(ctx context.Context, path, runner string, forUpdate bool) ([]byte, error) {
	if !forUpdate {
		path += "?includeFields=metadata.name,metadata.namespace,status.runner"
	}
	response, err := a.upstream.Do(ctx, http.MethodGet, path, nil, nil)
	if err != nil || response == nil {
		return nil, &Error{Status: http.StatusServiceUnavailable, Message: "Job lookup unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, &Error{Status: http.StatusNotFound, Message: "Job not found"}
	}
	if response.StatusCode != http.StatusOK {
		return nil, &Error{Status: http.StatusServiceUnavailable, Message: "Job lookup unavailable"}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, a.bodyLimit+1))
	if err != nil || int64(len(raw)) > a.bodyLimit {
		return nil, &Error{Status: http.StatusBadGateway, Message: "invalid Job response"}
	}
	var object struct {
		Status struct {
			Runner string `json:"runner"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Message: "invalid Job response"}
	}
	if object.Status.Runner != runner {
		return nil, deny("Job is not assigned to Runner")
	}
	if !forUpdate {
		return nil, nil
	}
	return raw, nil
}

func HasWatch(query url.Values) bool {
	for _, value := range query["watch"] {
		if !strings.EqualFold(value, "false") {
			return true
		}
	}
	return false
}

// Authorize evaluates identity, service grants and object ownership without HTTP context mutation.
func (a *Authorizer) Authorize(ctx context.Context, who identity.Principal, request Request, rule Rule) (Result, error) {
	if !rule.Identities.Allows(who) {
		return Result{}, deny("identity is not allowed for this route")
	}
	if rule.UserJWTOnly && (!who.IsUser() || who.AuthenticationMethod != identity.AuthenticationJWT) {
		return Result{}, deny("user login token required")
	}
	if who.Type == identity.ServiceType {
		return Result{}, authorizeService(who, request.Project, rule)
	}
	access := rule.ObjectAccess
	if request.Watch {
		access = rule.WatchAccess
	}
	return a.authorizeObject(ctx, who, request, access)
}

func (a *Authorizer) authorizeObject(ctx context.Context, who identity.Principal, request Request, access ObjectAccess) (Result, error) {
	switch access {
	case ObjectAccessNone:
		return Result{}, nil
	case RunnerSelf:
		if who.Type == identity.RunnerType && request.Runner != who.Subject {
			return Result{}, deny("Runner identity mismatch")
		}
		return Result{}, nil
	case ProjectOwner, ProjectOwnerUpdate, ProjectMember:
		if a == nil || a.upstream == nil {
			return Result{}, &Error{Status: http.StatusInternalServerError, Message: "object authorization is unavailable"}
		}
		if access == ProjectOwnerUpdate {
			raw, err := a.authorizeProjectUpdate(ctx, who, request.Project)
			return Result{UpdateObject: raw}, err
		}
		if access == ProjectOwner {
			return Result{}, a.AuthorizeProjectOwner(ctx, who, request.Project)
		}
		return Result{}, a.AuthorizeProjectMember(ctx, who, request.Project)
	case AssignedJob:
		if who.Type != identity.RunnerType {
			return Result{}, nil
		}
		if a == nil || a.upstream == nil {
			return Result{}, &Error{Status: http.StatusInternalServerError, Message: "object authorization is unavailable"}
		}
		raw, err := a.ReadAssignedJob(ctx, request.Path, who.Subject, request.Method == http.MethodPatch)
		return Result{UpdateObject: raw}, err
	default:
		return Result{}, deny("object access is not allowed")
	}
}

func authorizeService(who identity.Principal, project string, rule Rule) error {
	if !rule.ServicePermission.IsValid() {
		return deny("service operation is not allowed")
	}
	grants := who.ServiceGrants
	if project == "" || grants == nil ||
		(!slices.Contains(grants.Projects, project) && !slices.Contains(grants.Projects, iamv1.MachineAccountAllProjects)) ||
		!slices.Contains(grants.Permissions, rule.ServicePermission) {
		return deny("service account lacks project or operation permission")
	}
	return nil
}

// Validate rejects inconsistent policy declarations before serving requests.
func (r Rule) Validate() error {
	if r.UserJWTOnly && r.Identities & ^AllowUsers != 0 {
		return fmt.Errorf("login-only routes must allow only User identities")
	}
	if r.Identities == 0 || r.Identities & ^(AllowAnonymous|AllowAuthenticated) != 0 {
		return fmt.Errorf("allowed identities are empty or unknown")
	}
	for _, access := range []ObjectAccess{r.ObjectAccess, r.WatchAccess} {
		switch access {
		case ObjectAccessNone, ProjectOwner, ProjectOwnerUpdate, ProjectMember, RunnerSelf, AssignedJob:
		default:
			return fmt.Errorf("unknown object access %q", access)
		}
	}
	if r.Identities&AllowService == 0 {
		if r.ServicePermission != "" {
			return fmt.Errorf("service grants require an allowed Service identity")
		}
	} else if !r.ServicePermission.IsValid() {
		return fmt.Errorf("Service access requires a valid ServicePermission")
	}
	return nil
}

// Allows checks identity admission without evaluating business permissions.
func (allowed RouteIdentities) Allows(who identity.Principal) bool {
	if who.Type == "" && who.Subject == "" {
		return allowed&AllowAnonymous != 0
	}
	if who.Subject == "" {
		return false
	}
	switch who.Type {
	case identity.UserType:
		switch who.Scope {
		case identity.UserScope:
			return allowed&AllowUser != 0
		case identity.OpsScope:
			return allowed&AllowOps != 0
		case identity.AdminScope:
			return allowed&AllowAdmin != 0
		}
	case identity.RunnerType:
		return allowed&AllowRunner != 0
	case identity.ServiceType:
		return allowed&AllowService != 0
	}
	return false
}
