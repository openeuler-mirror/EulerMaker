package buildinfo

import (
	"strings"

	ebsv1 "ebs-api/ebs/v1"
)

// bootstrapRepoURLs resolves external bootstrap repository base URLs for the
// build target. RpmRepo contentURLs are already complete and are not included.
func bootstrapRepoURLs(repos []ebsv1.BootstrapRepo, arch string) []string {
	urls := make([]string, 0, len(repos))
	for _, repo := range repos {
		urls = append(urls, strings.TrimRight(strings.TrimSpace(repo.Repo), "/")+"/"+arch)
	}
	return urls
}
