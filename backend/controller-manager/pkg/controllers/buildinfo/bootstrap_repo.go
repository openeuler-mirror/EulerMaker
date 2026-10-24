package buildinfo

import (
	"context"
	"fmt"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
)

// bootstrapRepoURLs appends the build architecture while preserving logical references. RpmRepo contentURLs are
// already complete and are not included.
func bootstrapRepoURLs(repos []ebsv1.BootstrapRepo, arch string) []string {
	urls := make([]string, 0, len(repos))
	for _, repo := range repos {
		urls = append(urls, strings.TrimRight(strings.TrimSpace(repo.URL), "/")+"/"+arch)
	}
	return urls
}

// resolveBootstrapRepoURLs shares RpmRepo resolution for metadata access only.
// Job payloads retain logical references so each Runner can select its own endpoint.
func (c *Controller) resolveBootstrapRepoURLs(ctx context.Context, round *reconcileRound, arch string) ([]string, error) {
	references := bootstrapRepoURLs(round.current.Spec.BootstrapRepos, arch)
	urls := make([]string, 0, len(references))
	for _, reference := range references {
		resolved, err := c.resolveContentURL(ctx, round, reference)
		if err != nil {
			return nil, fmt.Errorf("bootstrap repository %q: %w", reference, err)
		}
		urls = append(urls, resolved)
	}
	return urls, nil
}
