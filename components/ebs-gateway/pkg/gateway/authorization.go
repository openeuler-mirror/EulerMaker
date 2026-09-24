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
	if ident.IsOps() && route.resource == "buildresources" {
		return g.authorizeOps(r, route)
	}
	return g.authorizeProjectRequest(ctx, r, ident, route)
}

// handled=false means the resource rule is not conclusive, not that access is granted.
func (g *Gateway) authorizeResource(ctx context.Context, r *http.Request, ident Identity, route routeInfo) (authzDecision, bool, error) {
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
	if r.Method == http.MethodDelete && route.resource == "buildresources" && route.project == "default" && route.name == "default" && len(route.rest) == 0 {
		return authzDecision{}, true, fmt.Errorf("global default BuildResource cannot be deleted")
	}
	if validPath && len(parts) >= 3 && parts[0] == "projects" && parts[2] == "buildconfs" {
		return authzDecision{}, true, fmt.Errorf("BuildConf is cluster-scoped")
	}
	if validPath && len(parts) > 0 && parts[0] == "buildconfs" {
		decision, err := authorizeBuildConf(r, ident, parts)
		return decision, true, err
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

func authorizeBuildConf(r *http.Request, ident Identity, parts []string) (authzDecision, error) {
	// Public reads are handled before authorizeAndPrepare.
	valid := len(parts) == 1 && r.Method == http.MethodPost || len(parts) == 2 && parts[1] == "default" && (r.Method == http.MethodPut || r.Method == http.MethodPatch)
	if !valid || !ident.IsPrivileged() {
		return authzDecision{}, fmt.Errorf("BuildConf write requires ops or higher and a supported operation")
	}
	return authzDecision{}, nil
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
		if route.resource == "buildresources" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			return authzDecision{}, fmt.Errorf("build resource access is read-only for project users")
		}
		if project.Labels[ownerUserLabel] != ident.Subject && r.Method == http.MethodDelete {
			return authzDecision{}, fmt.Errorf("project member cannot delete resources")
		}
		return authzDecision{}, nil
	}

	return authzDecision{}, fmt.Errorf("access denied")
}
