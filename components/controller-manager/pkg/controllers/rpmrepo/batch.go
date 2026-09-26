package rpmrepo

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	ebsv1 "ebs-api/ebs/v1"
)

// candidate is one succeeded Job eligible for materialization; its manifest is checked by Artifact Manager.
type candidate struct {
	uid       string
	name      string
	specName  string
	createdAt int64
}

// sortCandidates orders inputs by creation time and name so a retry selects the same batch.
func sortCandidates(values []candidate) {
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].createdAt != values[j].createdAt {
			return values[i].createdAt < values[j].createdAt
		}
		if values[i].name != values[j].name {
			return values[i].name < values[j].name
		}
		return false
	})
}

// selectBatch keeps at most one Job per spec, in stable order, up to the Job count limit.
func selectBatch(candidates []candidate, maxJobs int) []candidate {
	ordered := append([]candidate(nil), candidates...)
	sortCandidates(ordered)
	seenSpecs := make(map[string]struct{}, len(ordered))
	selection := make([]candidate, 0, min(len(ordered), maxJobs))
	for i := range ordered {
		item := ordered[i]
		if _, exists := seenSpecs[item.specName]; exists {
			continue
		}
		if len(selection) >= maxJobs {
			break
		}
		seenSpecs[item.specName] = struct{}{}
		selection = append(selection, item)
	}
	return selection
}

// repositoryUID reproduces the Artifact Manager identity: SHA-256 over length prefixed project, build name and
// base repository UID followed by the length prefixed Job names in ascending order.
func repositoryUID(project, buildName, baseRepositoryUID string, inputs []ebsv1.RepositoryInput) (string, error) {
	if project == "" || buildName == "" {
		return "", fmt.Errorf("repository UID requires project and build name")
	}
	if len(inputs) == 0 {
		return "", fmt.Errorf("repository UID requires at least one input")
	}
	ordered := append([]ebsv1.RepositoryInput(nil), inputs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].JobName < ordered[j].JobName })
	hash := sha256.New()
	writeLengthPrefixed := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	writeLengthPrefixed(project)
	writeLengthPrefixed(buildName)
	writeLengthPrefixed(baseRepositoryUID)
	for _, item := range ordered {
		writeLengthPrefixed(item.JobName)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// repositoryInputs maps selected candidates onto the frozen checkpoint inputs.
func repositoryInputs(selection []candidate) []ebsv1.RepositoryInput {
	inputs := make([]ebsv1.RepositoryInput, 0, len(selection))
	for _, item := range selection {
		inputs = append(inputs, ebsv1.RepositoryInput{JobName: item.name, SpecName: item.specName})
	}
	return inputs
}

// repositoryRequests maps frozen inputs onto the Artifact Manager request, ordered by Job name.
func repositoryRequests(inputs []ebsv1.RepositoryInput) []ManifestReference {
	ordered := append([]ebsv1.RepositoryInput(nil), inputs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].JobName < ordered[j].JobName })
	manifests := make([]ManifestReference, 0, len(ordered))
	for _, item := range ordered {
		manifests = append(manifests, ManifestReference{JobName: item.JobName})
	}
	return manifests
}

// unionSortedNames merges the already published source Job names with the promoted batch.
func unionSortedNames(existing []string, batch []ebsv1.RepositoryInput) []string {
	seen := make(map[string]struct{}, len(existing)+len(batch))
	result := make([]string, 0, len(existing)+len(batch))
	for _, value := range existing {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	for _, item := range batch {
		if item.JobName == "" {
			continue
		}
		if _, exists := seen[item.JobName]; exists {
			continue
		}
		seen[item.JobName] = struct{}{}
		result = append(result, item.JobName)
	}
	sort.Strings(result)
	return result
}

func unionSortedStrings(existing []string, value string) []string {
	set := make(map[string]struct{}, len(existing)+1)
	for _, uid := range existing {
		if uid != "" {
			set[uid] = struct{}{}
		}
	}
	if value != "" {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for uid := range set {
		result = append(result, uid)
	}
	sort.Strings(result)
	return result
}
