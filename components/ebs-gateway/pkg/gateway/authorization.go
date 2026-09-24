package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type authzDecision struct {
	handle http.HandlerFunc
}

// Resource-specific restrictions must run before the Admin/System fallback.
func (g *Gateway) authorizeAndPrepare(ctx context.Context, r *http.Request, ident Identity) (authzDecision, error) {
	route := parseRoute(r.URL.Path)
	if decision, handled, err := g.authorizeResource(ctx, r, ident, route); handled || err != nil {
		return decision, err
	}
	if ident.IsSystem() || ident.IsAdmin() {
		return g.prepareAdministrativeRequest(ctx, r, route)
	}
	if ident.IsRunner() {
		return g.authorizeRunner(ctx, r, ident, route)
	}
	return g.authorizeProjectRequest(ctx, r, ident, route)
}

// handled=false means the resource rule is not conclusive, not that access is granted.
func (g *Gateway) authorizeResource(ctx context.Context, r *http.Request, ident Identity, route routeInfo) (authzDecision, bool, error) {
	if strings.HasPrefix(r.URL.Path, apiPrefix+"/projects/") && strings.Contains(r.URL.Path, "/configs") {
		return authzDecision{}, true, fmt.Errorf("Config is cluster-scoped")
	}
	if r.URL.Path == apiPrefix+"/configs" || strings.HasPrefix(r.URL.Path, apiPrefix+"/configs/") {
		decision, err := authorizeConfig(r, ident)
		return decision, true, err
	}
	if route.resource == "builds" && (r.Method == http.MethodPut || r.Method == http.MethodPatch) {
		return authzDecision{}, true, fmt.Errorf("Build updates are not available through Gateway")
	}
	parts, validPath := ebsAPIPathParts(r.URL.Path)
	if validPath && len(parts) >= 3 && parts[0] == "projects" && parts[2] == "scripts" {
		return authzDecision{}, true, fmt.Errorf("Script is cluster-scoped")
	}
	if r.URL.Path == apiPrefix+"/scripts" || strings.HasPrefix(r.URL.Path, apiPrefix+"/scripts/") {
		decision, err := authorizeScript(r, ident)
		return decision, true, err
	}
	if route.resource == "jobs" {
		if len(route.rest) == 1 && route.rest[0] == "abort" {
			decision, err := g.authorizeJobAbort(ctx, r, ident, route)
			return decision, true, err
		}
		if len(route.rest) > 0 && route.rest[0] == "status" && r.Method != http.MethodGet && r.Method != http.MethodHead && !ident.IsSystem() && !ident.IsRunner() {
			return authzDecision{}, true, fmt.Errorf("Job status write requires system or assigned Runner identity")
		}
	}
	if route.resource == "runners" {
		if ident.IsPrivileged() {
			return authzDecision{}, true, nil
		}
		if ident.IsRunner() {
			decision, err := g.authorizeRunner(ctx, r, ident, route)
			return decision, true, err
		}
		return authzDecision{}, true, fmt.Errorf("runner api requires operations privileges or runner identity")
	}
	return authzDecision{}, false, nil
}

func (g *Gateway) authorizeJobAbort(ctx context.Context, r *http.Request, ident Identity, route routeInfo) (authzDecision, error) {
	if ident.IsRunner() || ident.IsSystem() || !(ident.IsUser() || ident.IsOps() || ident.IsAdmin()) || route.project == "" || route.name == "" {
		return authzDecision{}, fmt.Errorf("Job abort requires a user identity and project-scoped Job")
	}
	project, err := g.getProject(ctx, route.project)
	if err != nil {
		return authzDecision{}, err
	}
	if !projectAllowsUser(project, ident.Subject) {
		return authzDecision{}, fmt.Errorf("project access denied")
	}
	if r.Method != http.MethodPost {
		return authzDecision{handle: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}}, nil
	}
	return authzDecision{handle: g.jobAbortHandler(ident)}, nil
}

func (g *Gateway) prepareAdministrativeRequest(ctx context.Context, r *http.Request, route routeInfo) (authzDecision, error) {
	if route.resource == "projects" && route.project == "" && r.Method == http.MethodPost {
		if err := g.validateSystemProjectOwner(ctx, r); err != nil {
			return authzDecision{}, err
		}
	}
	return authzDecision{}, nil
}

func (g *Gateway) authorizeProjectRequest(ctx context.Context, r *http.Request, ident Identity, route routeInfo) (authzDecision, error) {
	if route.resource == "" {
		return authzDecision{}, fmt.Errorf("unsupported ebs api path")
	}

	if route.project == "" && isProjectScopedResource(route.resource) {
		return authzDecision{}, fmt.Errorf("global %s api requires system scope", route.resource)
	}

	if route.resource == "projects" && route.project == "" {
		return g.handleProjectCollection(ctx, r, ident)
	}

	if route.resource == "projects" && route.project != "" {
		project, err := g.getProject(ctx, route.project)
		if err != nil {
			return authzDecision{}, err
		}
		if !projectAllowsUser(project, ident.Subject) {
			return authzDecision{}, fmt.Errorf("project access denied")
		}
		if project.Labels[ownerUserLabel] != ident.Subject && r.Method != http.MethodGet && r.Method != http.MethodHead {
			return authzDecision{}, fmt.Errorf("only project owner can modify project")
		}
		if isProjectObjectWrite(r.Method, route) {
			if err := g.protectProjectAccessLabels(r, ident, project); err != nil {
				return authzDecision{}, err
			}
		}
		return authzDecision{}, nil
	}

	if route.project != "" {
		project, err := g.getProject(ctx, route.project)
		if err != nil {
			return authzDecision{}, err
		}
		if !projectAllowsUser(project, ident.Subject) {
			return authzDecision{}, fmt.Errorf("project access denied")
		}
		if project.Labels[ownerUserLabel] != ident.Subject && r.Method == http.MethodDelete {
			return authzDecision{}, fmt.Errorf("project member cannot delete resources")
		}
		return authzDecision{}, nil
	}

	return authzDecision{}, fmt.Errorf("access denied")
}
