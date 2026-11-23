package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
)

type Manifest struct {
	Path       string
	PackageRef string
	Data       []byte
}

type Change struct {
	Name   string             `json:"name"`
	Before *ebsv1.PackageRepo `json:"before,omitempty"`
	After  *ebsv1.PackageRepo `json:"after,omitempty"`
}

type Difference struct {
	Added    int      `json:"added"`
	Removed  int      `json:"removed"`
	Modified int      `json:"modified"`
	Changes  []Change `json:"changes,omitempty"`
}

func buildList(b Binding, files []Manifest, defaultRef string) ([]ebsv1.PackageRepo, error) {
	var repos []ebsv1.PackageRepo
	total := 0
	if len(files) == 0 || len(files) > maxFiles {
		return nil, fmt.Errorf("manifest file count outside limits")
	}
	for _, file := range files {
		total += len(file.Data)
		if len(file.Data) > maxYAMLBytes || total > maxTotalBytes {
			return nil, fmt.Errorf("manifest size limit exceeded")
		}
		value, err := decodeYAML(file.Data)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", file.Path, err)
		}
		items, ok := value.(map[string]any)["packages"].([]any)
		if !ok {
			return nil, fmt.Errorf("manifest %s requires a packages array", file.Path)
		}
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("manifest %s contains an invalid package", file.Path)
			}
			name, ok := entry["name"].(string)
			if !ok || !safeName.MatchString(name) {
				return nil, fmt.Errorf("manifest %s contains an invalid package name", file.Path)
			}
			repos = append(repos, ebsv1.PackageRepo{Name: name, URL: strings.ReplaceAll(b.PackageURLTemplate, "{name}", url.PathEscape(name)), Ref: file.PackageRef})
			if len(repos) > 100000 {
				return nil, fmt.Errorf("manifest package entry limit exceeded")
			}
		}
	}
	repos, err := normalize(repos)
	if err != nil {
		return nil, err
	}
	if len(repos) > maxPackages {
		return nil, fmt.Errorf("package count limit exceeded")
	}
	for _, repo := range repos {
		if repo.Ref == "" && !validRef(defaultRef) {
			return nil, fmt.Errorf("Project defaultRef is required for packages without an explicit reference")
		}
	}
	return repos, nil
}

func normalize(repos []ebsv1.PackageRepo) ([]ebsv1.PackageRepo, error) {
	byName := make(map[string]ebsv1.PackageRepo, len(repos))
	for _, r := range repos {
		if !safeName.MatchString(r.Name) {
			return nil, fmt.Errorf("invalid package name")
		}
		if _, err := httpURL(r.URL); err != nil {
			return nil, fmt.Errorf("package %s requires an HTTP(S) URL", r.Name)
		}
		if r.Ref != "" && !validRef(r.Ref) || r.CommitID != "" && !commitID.MatchString(r.CommitID) {
			return nil, fmt.Errorf("package %s has invalid ref or commitID", r.Name)
		}
		if old, exists := byName[r.Name]; exists && old != r {
			return nil, fmt.Errorf("conflicting package %s", r.Name)
		}
		byName[r.Name] = r
	}
	out := make([]ebsv1.PackageRepo, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b ebsv1.PackageRepo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func difference(old, next []ebsv1.PackageRepo) Difference {
	d := Difference{}
	before, after := map[string]ebsv1.PackageRepo{}, map[string]ebsv1.PackageRepo{}
	for _, r := range old {
		before[r.Name] = r
	}
	for _, r := range next {
		after[r.Name] = r
	}
	for name, r := range before {
		n, exists := after[name]
		if !exists {
			d.Removed++
			d.Changes = append(d.Changes, Change{Name: name, Before: &r})
		} else if r != n {
			d.Modified++
			d.Changes = append(d.Changes, Change{Name: name, Before: &r, After: &n})
		}
	}
	for name, r := range after {
		if _, exists := before[name]; !exists {
			d.Added++
			d.Changes = append(d.Changes, Change{Name: name, After: &r})
		}
	}
	slices.SortFunc(d.Changes, func(a, b Change) int { return strings.Compare(a.Name, b.Name) })
	return d
}

func equalList(a, b []ebsv1.PackageRepo) bool {
	x, e1 := normalize(a)
	y, e2 := normalize(b)
	return e1 == nil && e2 == nil && reflect.DeepEqual(x, y)
}
