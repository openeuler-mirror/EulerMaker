package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func configReadName(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead || hasWatchRequest(r) {
		return "", false
	}
	parts, ok := ebsAPIPathParts(r.URL.Path)
	if !ok || len(parts) != 2 || parts[0] != "configs" || !validPathSegment(parts[1]) {
		return "", false
	}
	return parts[1], true
}

// A named read is fetched once; authorization and the returned body use the
// same version of the object, so a visibility change cannot race a second GET.
func (g *Gateway) serveConfigRead(w http.ResponseWriter, r *http.Request, ident Identity, name string) {
	body, status, _, err := g.upstreamRequest(r.Context(), http.MethodGet, r.URL.Path, nil, nil)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}
	var obj struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Visibility string `json:"visibility"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.APIVersion != "ebs/v1" || obj.Kind != "Config" || obj.Metadata.Name != name {
		http.Error(w, "invalid upstream Config response", http.StatusBadGateway)
		return
	}
	if obj.Spec.Visibility != "Public" && !(obj.Spec.Visibility == "OpsOnly" && ident.IsPrivileged()) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.Error(w, "Config access denied", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func authorizeConfig(r *http.Request, ident Identity) (authzDecision, error) {
	parts, ok := ebsAPIPathParts(r.URL.Path)
	if !ok || len(parts) < 1 || len(parts) > 2 || parts[0] != "configs" || len(parts) == 2 && !validPathSegment(parts[1]) || hasWatchRequest(r) {
		return authzDecision{}, fmt.Errorf("unsupported Config API operation")
	}
	if !ident.IsPrivileged() {
		return authzDecision{}, fmt.Errorf("Config access denied")
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if len(parts) == 1 {
			return authzDecision{}, nil
		}
	case http.MethodPost:
		if len(parts) == 1 {
			return authzDecision{}, nil
		}
	case http.MethodPut, http.MethodPatch:
		if len(parts) == 2 {
			return authzDecision{}, nil
		}
	}
	return authzDecision{}, fmt.Errorf("unsupported Config API operation")
}
