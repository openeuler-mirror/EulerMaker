package gateway

import (
	"fmt"
	"net/http"
)

// Script is a global, authenticated resource. Check it before the general
// Admin/System bypass so unsupported operations remain unavailable to all roles.
func authorizeScript(r *http.Request, ident Identity) (authzDecision, error) {
	parts, ok := ebsAPIPathParts(r.URL.Path)
	if !ok || len(parts) < 1 || len(parts) > 2 || parts[0] != "scripts" || (len(parts) == 2 && !validPathSegment(parts[1])) || hasWatchRequest(r) {
		return authzDecision{}, fmt.Errorf("unsupported Script API operation")
	}
	privileged := ident.IsOps() || ident.IsAdmin() || ident.IsSystem()
	allowed := false
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		allowed = privileged || ident.IsUser()
		if ident.IsRunner() {
			allowed = len(parts) == 2 && ident.Runner != "" && ident.Subject == ident.Runner
		}
	case http.MethodPost:
		allowed = len(parts) == 1 && privileged
	case http.MethodPut, http.MethodPatch:
		allowed = len(parts) == 2 && privileged
	}
	if !allowed {
		return authzDecision{}, fmt.Errorf("Script access denied: writes require ops or higher; only supported reads are allowed")
	}
	injectIdentityHeaders(r, ident)
	return authzDecision{}, nil
}
