package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
)

func sameProjectType(labels map[string]any, old map[string]string) bool {
	value, exists := labels[projectTypeLabel]
	previous, hadLabel := old[projectTypeLabel]
	if !hadLabel {
		return !exists || value == "personal"
	}
	return exists && value == previous
}

// Evaluate the complete patch so replacing metadata/labels and move/copy cannot
// bypass classification protection. Pin the resulting PUT to the version read.
func (g *Gateway) prepareProjectTypePatch(r *http.Request, old projectInfo) error {
	data, err := readAndRestoreBody(r, g.cfg.MaxRequestBodyBytes)
	if err != nil {
		return err
	}
	oldData, err := json.Marshal(old.Object)
	if err != nil {
		return err
	}
	var candidateData []byte
	switch strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) {
	case "application/merge-patch+json":
		candidateData, err = jsonpatch.MergePatch(oldData, data)
	case "application/json-patch+json":
		var patch jsonpatch.Patch
		patch, err = jsonpatch.DecodePatch(data)
		if err == nil {
			candidateData, err = patch.Apply(oldData)
		}
	default:
		return fmt.Errorf("unsupported project patch type")
	}
	if err != nil || int64(len(candidateData)) > g.cfg.MaxRequestBodyBytes {
		return fmt.Errorf("invalid project patch")
	}
	candidate, err := decodeObject(candidateData)
	if err != nil {
		return err
	}
	if !sameProjectType(ensureLabels(candidate), old.Labels) {
		return fmt.Errorf("only ops or higher can modify project type")
	}
	metadata, _ := candidate["metadata"].(map[string]any)
	oldMetadata, _ := old.Object["metadata"].(map[string]any)
	version, _ := oldMetadata["resourceVersion"].(string)
	if version == "" || metadata["resourceVersion"] != version {
		return fmt.Errorf("project patch requires unchanged resourceVersion")
	}
	r.Method = http.MethodPut
	r.Header.Set("Content-Type", "application/json")
	return writeJSONObject(r, candidate)
}
