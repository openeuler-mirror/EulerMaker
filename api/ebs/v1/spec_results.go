package v1

import (
	"sort"
	"strings"
)

// SpecResultRepo extracts repository ownership from a result key.
func SpecResultRepo(key string) string {
	repo, _, _ := strings.Cut(key, "/")
	return repo
}

// FailedPackages derives repositories requiring another build from persisted results.
func (s SpecResults) FailedPackages() []string {
	repos := map[string]bool{}
	for key, result := range s {
		if result.Status.IsTerminal() && result.Status != SpecSucceeded {
			repos[SpecResultRepo(key)] = true
		}
	}
	names := make([]string, 0, len(repos))
	for name := range repos {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Entries returns parsed entries indexed by their RPM spec names for dependency processing.
func (s SpecResults) Entries() map[string]SpecResult {
	entries := make(map[string]SpecResult, len(s))
	for _, result := range s {
		if result.SpecName != "" && result.Status != SpecPrepareFailed {
			entries[result.SpecName] = result
		}
	}
	return entries
}

// Lookup finds a parsed spec without assuming that its Name equals its source filename.
func (s SpecResults) Lookup(name string) (SpecResult, bool) {
	for _, result := range s {
		if result.SpecName == name && result.Status != SpecPrepareFailed {
			return result, true
		}
	}
	return SpecResult{}, false
}

func (s SpecResults) Entry(name string) SpecResult {
	result, _ := s.Lookup(name)
	return result
}

// Set updates an existing parsed entry; callers must register its source key before dispatch.
func (s SpecResults) Set(name string, result SpecResult) {
	for key, existing := range s {
		if existing.SpecName == name && existing.Status != SpecPrepareFailed {
			result.SpecName = name
			s[key] = result
			return
		}
	}
}

func (s SpecResults) Len() int { return len(s) }
