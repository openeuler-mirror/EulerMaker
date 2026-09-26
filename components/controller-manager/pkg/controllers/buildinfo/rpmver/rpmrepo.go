// rpmrepo.go selects RPM providers and checks dependency availability.
package rpmver

import (
	"sort"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
)

// ProviderSelection contains the selected provider, normalized RPM name, and
// selection reason.
type ProviderSelection struct {
	Provider ProvideEntry
	RPMName  string
	Reason   SelectionReason
}

type SelectionReason string

const (
	SelectionSingle         SelectionReason = "Single"
	SelectionPrefer         SelectionReason = "Prefer"
	SelectionHighestVersion SelectionReason = "HighestVersion"
)

// FindProvider searches the RpmRepo layer first, then bootstrap layers in
// order, and returns the first matching provider.
func (s *RpmMetaSources) FindProvider(name string, constraint ebsv1.VersionConst, prefer []string) (ProviderSelection, bool) {
	for _, src := range s.layers() {
		if selected := GetProvideInfo(name, src, prefer, constraint); selected.Provider.SpecName != "" {
			return selected, true
		}
	}
	return ProviderSelection{}, false
}

// GetProvideInfo filters one source by version, then selects the sole
// candidate, the first preferred candidate, or the highest version. A zero
// result means no provider was selected.
func GetProvideInfo(name string, src *RpmMetaSource, prefer []string, constraint ebsv1.VersionConst) ProviderSelection {
	candidates := src.ProvidesInfo[name]
	if len(candidates) == 0 {
		return ProviderSelection{}
	}
	// Every version constraint must match.
	filtered := make(map[string]ProvideEntry, len(candidates))
	for rpmName, entry := range candidates {
		ok, err := VersionSatisfies(entry.Version, constraint)
		if err != nil || !ok {
			continue
		}
		filtered[rpmName] = entry
	}
	switch len(filtered) {
	case 0:
		return ProviderSelection{}
	case 1:
		for rpmName, entry := range filtered {
			return ProviderSelection{Provider: entry, RPMName: baseName(rpmName), Reason: SelectionSingle}
		}
	}
	// Sort candidate names for deterministic tie-breaking.
	names := make([]string, 0, len(filtered))
	for rpmName := range filtered {
		names = append(names, rpmName)
	}
	sort.Strings(names)
	// Match prefer entries in order against normalized RPM names.
	for _, want := range prefer {
		for _, rpmName := range names {
			if baseName(rpmName) == want {
				return ProviderSelection{Provider: filtered[rpmName], RPMName: want, Reason: SelectionPrefer}
			}
		}
	}
	// Fall back to the highest version; an empty version invalidates the choice.
	bestName := names[0]
	best := filtered[bestName]
	if best.Version == "" {
		return ProviderSelection{}
	}
	for _, rpmName := range names[1:] {
		entry := filtered[rpmName]
		if entry.Version == "" {
			return ProviderSelection{}
		}
		if less, err := VRCompare(best.Version, OpLT, entry.Version); err == nil && less {
			best = entry
			bestName = rpmName
		}
	}
	return ProviderSelection{Provider: best, RPMName: baseName(bestName), Reason: SelectionHighestVersion}
}

// Available checks each layer for a matching provide, then for an RPM with
// the requested name and version. A miss continues to the next layer.
func (s *RpmMetaSources) Available(name string, constraint ebsv1.VersionConst) bool {
	for _, src := range s.layers() {
		if GetProvideInfo(name, src, nil, constraint).Provider.SpecName != "" {
			return true
		}
		if meta, ok := src.RpmByName[name]; ok && meta.Version != "" {
			if ok, err := VersionSatisfies(meta.Version, constraint); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// RepoRequires merges requirements from the spec's RPMs in the RpmRepo layer.
// It returns nil when none are present and processes RPMs in name order.
func (s *RpmMetaSources) RepoRequires(spec string) map[string]ebsv1.VersionConst {
	if s.RepoLayer == nil {
		return nil
	}
	names := make([]string, 0, len(s.RepoLayer.RpmByName))
	for name, rpm := range s.RepoLayer.RpmByName {
		if rpm.SpecName == spec {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var out map[string]ebsv1.VersionConst
	for _, name := range names {
		for reqName, vc := range s.RepoLayer.RpmByName[name].Requires {
			if out == nil {
				out = map[string]ebsv1.VersionConst{}
			}
			out[reqName] = vc
		}
	}
	return out
}

// baseName normalizes a candidate key to its @ base name (a key without "@"
// is its own base name).
func baseName(key string) string {
	if i := strings.Index(key, "@"); i >= 0 {
		return key[:i]
	}
	return key
}
