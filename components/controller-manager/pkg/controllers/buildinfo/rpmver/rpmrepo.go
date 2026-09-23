// rpmrepo.go implements the version-aware provider selection chain and the
// RPM availability two-stage match (design 16.1), plus the Job payload Repo
// string assembly (design 15.3.1 / 7.2.3).
package rpmver

import (
	"sort"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
)

// FindProvider runs the 16.1 selection chain layer by layer (RpmRepo layer
// first, then BootstrapRepo layers in declaration order) with layer
// short-circuit: the first layer with a hit wins. The bool reports a hit.
func (s *RpmMetaSources) FindProvider(name string, constraint ebsv1.VersionConst, prefer []string) (ProvideEntry, bool) {
	for _, src := range s.layers() {
		if entry := GetProvideInfo(name, src, prefer, constraint); entry.SpecName != "" {
			return entry, true
		}
	}
	return ProvideEntry{}, false
}

// GetProvideInfo runs the four-step selection chain within one source
// (design 16.1): version-constraint filter -> single candidate direct pick ->
// prefer list order match (@ base-name normalized) -> highest version. The
// zero ProvideEntry means a miss; any empty candidate version on the
// highest-version step fails the whole provide (dirty data, wait next round).
func GetProvideInfo(name string, src *RpmMetaSource, prefer []string, constraint ebsv1.VersionConst) ProvideEntry {
	candidates := src.ProvidesInfo[name]
	if len(candidates) == 0 {
		return ProvideEntry{}
	}
	// Step 1: version-constraint filter (every operator AND; an empty
	// constraint set passes all).
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
		return ProvideEntry{}
	case 1:
		// Step 2: single candidate — direct pick.
		for _, entry := range filtered {
			return entry
		}
	}
	// Deterministic iteration for steps 3 and 4.
	names := make([]string, 0, len(filtered))
	for rpmName := range filtered {
		names = append(names, rpmName)
	}
	sort.Strings(names)
	// Step 3: prefer hit — candidates normalize to their @ base name; the
	// first prefer item matching any candidate's base name wins.
	for _, want := range prefer {
		for _, rpmName := range names {
			if baseName(rpmName) == want {
				return filtered[rpmName]
			}
		}
	}
	// Step 4: highest version — vrCompare(current best, LT, candidate)
	// replaces the best. Any empty candidate version fails the whole provide.
	best := filtered[names[0]]
	if best.Version == "" {
		return ProvideEntry{}
	}
	for _, rpmName := range names[1:] {
		entry := filtered[rpmName]
		if entry.Version == "" {
			return ProvideEntry{}
		}
		if less, err := VRCompare(best.Version, OpLT, entry.Version); err == nil && less {
			best = entry
		}
	}
	return best
}

// Available reports whether the named RPM with the given constraint is
// available in the layered sources (design 16.1 rpmAvailable): stage one is
// the providesInfo reverse lookup (version filter built in, prefer nil);
// stage two falls back to the rpm name direct index (self-provide). A dirty
// fallback entry (empty version) only ends the current source's fallback —
// later sources are still tried; every source failing means unavailable.
func (s *RpmMetaSources) Available(name string, constraint ebsv1.VersionConst) bool {
	for _, src := range s.layers() {
		if GetProvideInfo(name, src, nil, constraint).SpecName != "" {
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

// RepoRequires returns the merged install-time requirement set of one spec's
// own rpms (design 16.1 install edge input): RpmRepo layer entries whose
// specName == spec. Only the RpmRepo layer participates (bootstrap layers are
// external upstream). Returns nil when the layer or entries are absent. Rpms
// are merged in name order for determinism; the intersection merge with the
// explicit SpecDepend.requires set happens at the caller (16.1).
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
