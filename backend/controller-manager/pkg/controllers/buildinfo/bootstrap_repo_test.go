package buildinfo

import (
	"context"
	"controller-manager/pkg/serviceconfig"
	"reflect"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestBootstrapMetadataResolvesLogicalReferences(t *testing.T) {
	for _, single := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "single"}[single], func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			client.artifactEndpoints["art-01"] = "http://artifact-manager.art-01:8081"
			key := testNS + "/" + testBuild
			bi := testBuildInfoObj(ebsv1.BuildInfoPending)
			bi.Spec.BootstrapRepos = []ebsv1.BootstrapRepo{
				{URL: "artifact://art-01/repositories/base/"},
				{URL: "artifact:///repositories/fallback"},
				{URL: "https://mirror.example/base"},
			}
			round := &reconcileRound{key: key, current: bi, build: testBuildObj("single"), failures: c.newRoundFailures(key)}
			arch := round.build.Spec.Arch
			want := []string{"http://artifact-manager.art-01:8081/repositories/base/" + arch + "/repodata/repomd.xml", "http://artifact.example:8081/repositories/fallback/" + arch + "/repodata/repomd.xml", "https://mirror.example/base/" + arch + "/repodata/repomd.xml"}
			var fetched []string
			oldFetch := rpmMetaFetch
			t.Cleanup(func() { rpmMetaFetch = oldFetch })
			rpmMetaFetch = func(ctx context.Context, url string) ([]byte, error) {
				fetched = append(fetched, url)
				return singleMetadataFixture(ctx, url)
			}
			var err error
			if single {
				_, _, err = c.loadSinglePreferSources(context.Background(), round, "")
			} else {
				_, _, err = c.refreshRpmMetaSources(context.Background(), round)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(fetched) != 6 {
				t.Fatalf("metadata requests=%v", fetched)
			}
			for i, url := range want {
				if fetched[i*2] != url {
					t.Errorf("request=%q, want %q", fetched[i*2], url)
				}
			}
			if client.artifactReads != 1 {
				t.Errorf("service config reads=%d, want 1", client.artifactReads)
			}
			references := bootstrapRepoURLs(bi.Spec.BootstrapRepos, arch)
			expected := []string{"artifact://art-01/repositories/base/" + arch, "artifact:///repositories/fallback/" + arch, "https://mirror.example/base/" + arch}
			if !reflect.DeepEqual(repoPayloadURLs("", references), expected) {
				t.Fatalf("payload references changed: %v", references)
			}
		})
	}
}

func TestBootstrapLogicalReferenceFailure(t *testing.T) {
	for _, reference := range []string{"artifact:///repositories/../secret", "artifact:///other/repo", "artifact:///repositories/repo?token=x", "ftp://example/repo"} {
		c, _, _, _ := newTestController(t)
		bi := testBuildInfoObj(ebsv1.BuildInfoPending)
		bi.Spec.BootstrapRepos = []ebsv1.BootstrapRepo{{URL: reference}}
		_, err := c.resolveBootstrapRepoURLs(context.Background(), &reconcileRound{current: bi}, "x86_64")
		if err == nil || !deterministicRpmRepoError(err) {
			t.Errorf("reference %q: expected configuration failure, got %v", reference, err)
		}
	}
}

func TestBootstrapMissingInstancePreservesServiceConfigError(t *testing.T) {
	c, _, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoPending)
	bi.Spec.BootstrapRepos = []ebsv1.BootstrapRepo{{URL: "artifact://missing/repositories/base"}}
	_, err := c.resolveBootstrapRepoURLs(context.Background(), &reconcileRound{current: bi}, "x86_64")
	if !serviceconfig.IsError(err) {
		t.Fatalf("expected service configuration error, got %v", err)
	}
}
