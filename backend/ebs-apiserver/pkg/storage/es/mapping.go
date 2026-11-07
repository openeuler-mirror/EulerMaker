package es

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
)

type MappingChange struct {
	Path   string
	Kind   string
	Before any
	After  any
}

type MappingResult struct {
	Resource string
	Alias    string
	Index    string
	Status   string
	Message  string
	Changes  []MappingChange
	patch    map[string]any
}

type MappingReport struct {
	Results []MappingResult
}

// HasAdditions reports whether a preview contains changes that can be applied.
func (r MappingReport) HasAdditions() bool {
	for _, result := range r.Results {
		if result.Status == "Planned" {
			return true
		}
	}
	return false
}

// UpdateMappings previews the complete selection before optionally adding fields.
// A nil selection discovers known deployed resources; an explicit selection must exist.
func (c *Client) UpdateMappings(ctx context.Context, resources []string, apply bool) (MappingReport, error) {
	selected, err := selectMappingResources(resources)
	if err != nil {
		return MappingReport{}, err
	}
	report := MappingReport{}
	blocked := false
	for _, resource := range selected {
		result := MappingResult{Resource: resource, Alias: resourceIndex(resource)}
		current, missing, err := c.inspectMapping(ctx, &result)
		switch {
		case err != nil:
			result.Status, result.Message = "Failed", err.Error()
			blocked = true
		case missing:
			result.Status, result.Message = "Missing", "alias is not deployed"
			blocked = blocked || resources != nil
		default:
			if err := result.compare(current); err != nil {
				result.Status, result.Message = "Failed", err.Error()
			}
			blocked = blocked || result.Status == "Conflict" || result.Status == "Failed"
		}
		report.Results = append(report.Results, result)
	}
	if blocked {
		if apply {
			report.skipPending()
		}
		return report, fmt.Errorf("mapping preflight failed; no updates were submitted")
	}
	if !apply {
		return report, nil
	}
	for i := range report.Results {
		result := &report.Results[i]
		if result.Status != "Planned" {
			continue
		}
		if err := c.applyMapping(ctx, result); err != nil {
			report.skipPending()
			return report, fmt.Errorf("mapping update stopped at %s: %w", result.Resource, err)
		}
	}
	return report, nil
}

func (r *MappingReport) skipPending() {
	for i := range r.Results {
		if r.Results[i].Status == "Planned" {
			r.Results[i].Status = "NotAttempted"
		}
	}
}

func selectMappingResources(resources []string) ([]string, error) {
	selected := map[string]bool{}
	if resources == nil {
		for resource := range indices {
			selected[resource] = true
		}
	} else {
		if len(resources) == 0 {
			return nil, fmt.Errorf("resources must not be empty")
		}
		for _, resource := range resources {
			resource = strings.TrimSpace(resource)
			if _, ok := indices[resource]; !ok {
				return nil, fmt.Errorf("unknown mapping resource %q", resource)
			}
			selected[resource] = true
		}
	}
	return sortedKeys(selected), nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (r *MappingResult) compare(current map[string]any) error {
	var definition struct {
		Mappings map[string]any `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(mappingForResource(r.Resource)), &definition); err != nil {
		return fmt.Errorf("invalid built-in mapping for %s", r.Resource)
	}
	r.Changes = nil
	r.patch = compareMappingNode("", current, definition.Mappings, true, true, &r.Changes)
	sort.Slice(r.Changes, func(i, j int) bool { return r.Changes[i].Path < r.Changes[j].Path })
	r.Status = "Unchanged"
	if len(r.patch) > 0 {
		r.Status = "Planned"
	}
	for _, change := range r.Changes {
		if change.Kind == "Conflict" {
			r.Status = "Conflict"
		}
	}
	return nil
}

func mappingObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

// mappingParameters normalizes only defaults whose semantics are known here.
// Unknown parameters are compared literally rather than assumed equivalent.
func mappingParameters(node map[string]any, root bool, inheritedDynamic any) map[string]any {
	params := map[string]any{}
	for key, value := range node {
		if key != "properties" {
			params[key] = value
		}
	}
	if !root && params["type"] == nil && (node["properties"] != nil || node["enabled"] != nil || node["dynamic"] != nil) {
		params["type"] = "object"
	}
	if root || params["type"] == "object" || params["type"] == "nested" {
		if params["dynamic"] == nil {
			params["dynamic"] = inheritedDynamic
		}
		switch params["dynamic"] {
		case "true":
			params["dynamic"] = true
		case "false":
			params["dynamic"] = false
		}
		if !root && params["enabled"] == nil {
			params["enabled"] = true
		}
	}
	switch params["type"] {
	case "keyword", "date", "boolean", "byte", "short", "integer", "long", "float", "double", "half_float", "scaled_float", "ip", "text":
		if params["index"] == nil {
			params["index"] = true
		}
		if params["doc_values"] == nil && params["type"] != "text" {
			params["doc_values"] = true
		}
	}
	return params
}

func compareMappingNode(path string, current, desired map[string]any, currentDynamic, desiredDynamic any, changes *[]MappingChange) map[string]any {
	before := mappingParameters(current, path == "", currentDynamic)
	after := mappingParameters(desired, path == "", desiredDynamic)
	params := map[string]bool{}
	for key := range before {
		params[key] = true
	}
	for key := range after {
		params[key] = true
	}
	for _, key := range sortedKeys(params) {
		if !reflect.DeepEqual(before[key], after[key]) {
			*changes = append(*changes, MappingChange{Path: path + "@" + key, Kind: "Conflict", Before: before[key], After: after[key]})
		}
	}
	oldProperties, newProperties := mappingObject(current["properties"]), mappingObject(desired["properties"])
	added := map[string]any{}
	for _, name := range sortedKeys(newProperties) {
		fieldPath := name
		if path != "" {
			fieldPath = path + "." + name
		}
		oldField, exists := oldProperties[name]
		if !exists {
			added[name] = newProperties[name]
			*changes = append(*changes, MappingChange{Path: fieldPath, Kind: "Add", After: newProperties[name]})
			continue
		}
		child := compareMappingNode(fieldPath, mappingObject(oldField), mappingObject(newProperties[name]), before["dynamic"], after["dynamic"], changes)
		if len(child) > 0 {
			child["type"] = mappingParameters(mappingObject(oldField), false, before["dynamic"])["type"]
			added[name] = child
		}
	}
	for _, name := range sortedKeys(oldProperties) {
		if _, exists := newProperties[name]; !exists {
			fieldPath := name
			if path != "" {
				fieldPath = path + "." + name
			}
			*changes = append(*changes, MappingChange{Path: fieldPath, Kind: "Legacy", Before: oldProperties[name]})
		}
	}
	if len(added) == 0 {
		return nil
	}
	return map[string]any{"properties": added}
}

func (c *Client) inspectMapping(ctx context.Context, result *MappingResult) (map[string]any, bool, error) {
	var aliases map[string]struct {
		Aliases map[string]struct {
			Write bool `json:"is_write_index"`
		} `json:"aliases"`
	}
	err := c.mappingRequest(ctx, http.MethodGet, "/_alias/"+url.PathEscape(result.Alias), nil, &aliases)
	if missingMappingAlias(err, result.Alias) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, safeMappingError(err)
	}
	if len(aliases) != 1 {
		return nil, false, fmt.Errorf("alias must reference exactly one index")
	}
	var index string
	for name, entry := range aliases {
		if !entry.Aliases[result.Alias].Write {
			return nil, false, fmt.Errorf("alias requires is_write_index=true")
		}
		index = name
	}
	if result.Index != "" && result.Index != index {
		return nil, false, fmt.Errorf("alias target changed from %s to %s", result.Index, index)
	}
	result.Index = index
	var mappings map[string]struct {
		Mappings map[string]any `json:"mappings"`
	}
	if err := c.mappingRequest(ctx, http.MethodGet, "/"+url.PathEscape(index)+"/_mapping", nil, &mappings); err != nil {
		return nil, false, safeMappingError(err)
	}
	entry, ok := mappings[index]
	if !ok || len(mappings) != 1 || entry.Mappings == nil {
		return nil, false, fmt.Errorf("invalid mapping response for target index")
	}
	return entry.Mappings, false, nil
}

func missingMappingAlias(err error, alias string) bool {
	var status *HTTPError
	if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
		return false
	}
	var body struct {
		Error any `json:"error"`
	}
	if json.Unmarshal([]byte(status.Body), &body) != nil {
		return false
	}
	return body.Error == "alias ["+alias+"] missing"
}

// Error bodies may contain upstream diagnostics or credentials; never print them.
func safeMappingError(err error) error {
	var status *HTTPError
	if errors.As(err, &status) {
		return fmt.Errorf("Elasticsearch returned HTTP %d", status.StatusCode)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("Elasticsearch request failed or returned invalid JSON")
}

func (c *Client) mappingRequest(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.addr()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) applyMapping(ctx context.Context, result *MappingResult) error {
	current, missing, err := c.inspectMapping(ctx, result)
	if missing {
		err = fmt.Errorf("alias disappeared before update")
	}
	if err == nil {
		err = result.compare(current)
	}
	if err == nil && result.Status == "Conflict" {
		err = fmt.Errorf("mapping changed incompatibly before update")
	}
	if err != nil {
		result.Status, result.Message = "Failed", err.Error()
		return err
	}
	if result.Status == "Unchanged" {
		return nil
	}
	body, err := json.Marshal(result.patch)
	if err != nil {
		result.Status, result.Message = "Failed", "cannot encode mapping patch"
		return errors.New(result.Message)
	}
	var acknowledgement struct {
		Acknowledged bool `json:"acknowledged"`
	}
	writeErr := c.mappingRequest(ctx, http.MethodPut, "/"+url.PathEscape(result.Index)+"/_mapping", body, &acknowledgement)
	confirmed := *result
	current, missing, err = c.inspectMapping(ctx, &confirmed)
	if err == nil && !missing {
		err = confirmed.compare(current)
		if err == nil && confirmed.Status == "Unchanged" {
			result.Status = "Updated"
			return nil
		}
	}
	result.Status, result.Message = "Unconfirmed", "mapping update could not be confirmed; inspect current state before retrying"
	var status *HTTPError
	if errors.As(writeErr, &status) && status.StatusCode >= 400 && status.StatusCode < 500 {
		result.Status, result.Message = "Failed", safeMappingError(writeErr).Error()
	}
	return errors.New(result.Message)
}
