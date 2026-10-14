package rpmrepo

import (
	"context"
	"fmt"
	"sort"

	ebsv1 "ebs-api/ebs/v1"
)

// PublishPolicyInput carries every input the release decision may depend on.
type PublishPolicyInput struct {
	Project             string
	Build               *ebsv1.Build
	BuildInfo           *ebsv1.BuildInfo
	SourceRepositoryUID string
	TargetOS            string
	TargetArch          string
}

// PublishDecision is returned in one call so both answers share the same inputs.
type PublishDecision struct {
	Publish      bool
	ExcludeSpecs []string
}

type PublishPolicy interface {
	Decide(context.Context, PublishPolicyInput) (PublishDecision, error)
}

// DefaultPublishPolicy publishes everything unless the build target disables publishing. buildType never
// participates: this controller only sees keys dispatched from RpmRepo polling.
type DefaultPublishPolicy struct{}

func (DefaultPublishPolicy) Decide(_ context.Context, input PublishPolicyInput) (PublishDecision, error) {
	if input.Build == nil {
		return PublishDecision{}, fmt.Errorf("publish policy input requires a Build")
	}
	return PublishDecision{Publish: input.Build.Spec.BuildTarget.PublishFlag}, nil
}

// normalizeExcludeSpecs deduplicates and sorts the exclusion list so the frozen checkpoint and the submitted
// request always carry the same normalized form.
func normalizeExcludeSpecs(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
