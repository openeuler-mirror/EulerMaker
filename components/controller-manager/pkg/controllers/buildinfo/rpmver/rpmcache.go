// rpmcache.go implements the RpmMeta two-layer in-memory cache parsing and
// structures (design 15.10): per-source repomd.xml -> primary metadata
// two-step resolution, gzip decoding, RpmMeta extraction, and the RpmByName /
// ProvidesInfo indexes. The cache itself is a pure in-memory acceleration
// layer — nothing is persisted to status.
package rpmver

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
)

// ProvideEntry is one providesInfo leaf: the providing rpm's version and the
// spec that produces it (design 15.10).
type ProvideEntry struct {
	Version  string
	SpecName string
}

// RpmMeta is one parsed rpm entry (design 15.10; fields per data-models).
type RpmMeta struct {
	Name     string
	Version  string // epoch:version-release, version-constraint compare input
	SpecName string // derived from sourcerpm; rpm name fallback
	Provides map[string]string
	Requires map[string]ebsv1.VersionConst
}

// RpmMetaSource is the parse product of a single repository URL; sources are
// never merged so per-layer short-circuit query semantics are preserved.
type RpmMetaSource struct {
	URL          string
	RpmByName    map[string]RpmMeta
	ProvidesInfo map[string]map[string]ProvideEntry
}

// RpmMetaSources is one BuildInfo's layered cache: the RpmRepo layer plus the
// BootstrapRepo layers in declaration order (design 15.10).
type RpmMetaSources struct {
	RepoLayer      *RpmMetaSource
	BootstrapLayer []*RpmMetaSource
}

// maxMetadataBytes bounds one repository metadata response body and its
// decompressed size (design 15.10). primary.xml of large distributions can
// legitimately reach hundreds of MiB, so the cap is generous — its purpose is
// to stop an oversized or hostile payload (e.g. a gzip bomb) from ballooning
// controller memory under concurrent reconcile rounds, not to police normal
// repositories. The HTTP client timeout bounds time only, never size.
const maxMetadataBytes = 512 << 20

// Fetcher downloads a URL and returns the response body.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// HTTPFetcher returns a Fetcher backed by an *http.Client.
func HTTPFetcher(client *http.Client) Fetcher {
	return func(ctx context.Context, url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
		}
		return readLimited(resp.Body, url)
	}
}

// readLimited reads r fully but refuses bodies larger than maxMetadataBytes.
func readLimited(r io.Reader, what string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxMetadataBytes {
		return nil, fmt.Errorf("%s: body exceeds %d bytes", what, maxMetadataBytes)
	}
	return body, nil
}

// FailureKind classifies a source load failure (design 15.10 error
// semantics: download vs parse feed distinct E-29 reasons).
type FailureKind string

const (
	FailureDownload FailureKind = "Download"
	FailureParse    FailureKind = "Parse"
)

// SourceError is a repository metadata load failure with its kind and URL.
type SourceError struct {
	Kind FailureKind
	URL  string
	Err  error
}

func (e *SourceError) Error() string {
	return fmt.Sprintf("rpm meta source %s (%s): %v", e.URL, e.Kind, e.Err)
}

func (e *SourceError) Unwrap() error { return e.Err }

// ParseRepoSource downloads and parses one repository (design 15.10 two-step
// resolution): <baseURL>/repodata/repomd.xml yields the primary metadata
// location href, which is then downloaded (gzip decoded when *.gz) and parsed
// into RpmByName / ProvidesInfo.
//
// Arch selection: entries of the target arch plus noarch are indexed; on a
// same-name conflict the target arch wins ("同源内同名不同 arch 并存时取目标
// arch 条目"); on a same name+arch collision the highest version wins.
func ParseRepoSource(ctx context.Context, fetch Fetcher, baseURL, arch string) (*RpmMetaSource, error) {
	base := strings.TrimSuffix(baseURL, "/")
	repomdURL := base + "/repodata/repomd.xml"
	body, err := fetch(ctx, repomdURL)
	if err != nil {
		return nil, &SourceError{Kind: FailureDownload, URL: repomdURL, Err: err}
	}
	var md repomdXML
	if err := xml.Unmarshal(body, &md); err != nil {
		return nil, &SourceError{Kind: FailureParse, URL: repomdURL, Err: err}
	}
	href := ""
	for _, d := range md.Data {
		if d.Type == "primary" {
			href = d.Location.Href
			break
		}
	}
	if href == "" {
		return nil, &SourceError{Kind: FailureParse, URL: repomdURL, Err: fmt.Errorf("repomd has no primary data record")}
	}
	primaryURL := base + "/" + href
	body, err = fetch(ctx, primaryURL)
	if err != nil {
		return nil, &SourceError{Kind: FailureDownload, URL: primaryURL, Err: err}
	}
	if strings.HasSuffix(href, ".gz") {
		body, err = gunzip(body)
		if err != nil {
			return nil, &SourceError{Kind: FailureParse, URL: primaryURL, Err: err}
		}
	}
	var meta primaryXML
	if err := xml.Unmarshal(body, &meta); err != nil {
		return nil, &SourceError{Kind: FailureParse, URL: primaryURL, Err: err}
	}
	src := &RpmMetaSource{
		URL:          base,
		RpmByName:    map[string]RpmMeta{},
		ProvidesInfo: map[string]map[string]ProvideEntry{},
	}
	archByName := map[string]string{}
	for i := range meta.Packages {
		p := &meta.Packages[i]
		if p.Arch != arch && p.Arch != "noarch" {
			continue
		}
		rpm := RpmMeta{
			Name:     p.Name,
			Version:  joinVersion(p.Version.Epoch, p.Version.Ver, p.Version.Rel),
			SpecName: specNameFromSourceRpm(p.Format.SourceRpm, p.Name),
			Provides: map[string]string{},
			Requires: map[string]ebsv1.VersionConst{},
		}
		for _, e := range p.Format.Provides {
			rpm.Provides[e.Name] = joinVersion(e.Epoch, e.Ver, e.Rel)
		}
		for _, e := range p.Format.Requires {
			rpm.Requires[e.Name] = flagsToVersionConst(e.Flags, joinVersion(e.Epoch, e.Ver, e.Rel))
		}
		if existingArch, ok := archByName[rpm.Name]; ok {
			// Same-name conflict: the target arch beats noarch; within one
			// arch the highest version wins (compare failure keeps the first
			// entry — dirty data stays unavailable, 7.4.1 defense).
			switch {
			case p.Arch == arch && existingArch != arch:
				// target arch replaces noarch
			case p.Arch != arch && existingArch == arch:
				continue
			default:
				better, cmpErr := VRCompare(rpm.Version, OpGT, src.RpmByName[rpm.Name].Version)
				if cmpErr != nil || !better {
					continue
				}
			}
		}
		src.RpmByName[rpm.Name] = rpm
		archByName[rpm.Name] = p.Arch
	}
	// providesInfo secondary index (design 16.1): per source, never merged.
	for rpmName, rpm := range src.RpmByName {
		for provideName, version := range rpm.Provides {
			bucket := src.ProvidesInfo[provideName]
			if bucket == nil {
				bucket = map[string]ProvideEntry{}
				src.ProvidesInfo[provideName] = bucket
			}
			bucket[rpmName] = ProvideEntry{Version: version, SpecName: rpm.SpecName}
		}
	}
	return src, nil
}

// EnsureRepoLayer implements the RpmRepo layer refresh rule (design 15.10):
// an empty contentURL is a normal empty state (nil layer, no failure count);
// an unchanged URL reuses the cache without downloading; only a changed URL
// re-downloads and overwrites.
func (s *RpmMetaSources) EnsureRepoLayer(ctx context.Context, fetch Fetcher, contentURL, arch string) error {
	if contentURL == "" {
		s.RepoLayer = nil
		return nil
	}
	if s.RepoLayer != nil && s.RepoLayer.URL == strings.TrimSuffix(contentURL, "/") {
		return nil
	}
	src, err := ParseRepoSource(ctx, fetch, contentURL, arch)
	if err != nil {
		return err
	}
	s.RepoLayer = src
	return nil
}

// EnsureBootstrapLayers lazily parses the bootstrap repositories once, in
// declaration order, and reuses them for the BuildInfo lifetime (design
// 15.10: spec.bootstrapRepo never changes, no per-round refresh).
func (s *RpmMetaSources) EnsureBootstrapLayers(ctx context.Context, fetch Fetcher, urls []string, arch string) error {
	if s.BootstrapLayer != nil {
		return nil
	}
	layers := make([]*RpmMetaSource, 0, len(urls))
	for _, u := range urls {
		src, err := ParseRepoSource(ctx, fetch, u, arch)
		if err != nil {
			return err
		}
		layers = append(layers, src)
	}
	s.BootstrapLayer = layers
	return nil
}

// layers returns the query order: RpmRepo layer first, then BootstrapRepo
// layers in declaration order (design 15.10).
func (s *RpmMetaSources) layers() []*RpmMetaSource {
	var out []*RpmMetaSource
	if s.RepoLayer != nil {
		out = append(out, s.RepoLayer)
	}
	out = append(out, s.BootstrapLayer...)
	return out
}

// specNameFromSourceRpm derives the producing spec name by stripping the
// -<version>-<release>.src.rpm suffix; missing or anomalous input falls back
// to the rpm name (design 15.10, same rule as artifact-manager 9.3.3).
func specNameFromSourceRpm(sourceRpm, rpmName string) string {
	s := strings.TrimSuffix(sourceRpm, ".src.rpm")
	if s == sourceRpm {
		return rpmName
	}
	i := strings.LastIndex(s, "-")
	if i <= 0 {
		return rpmName
	}
	j := strings.LastIndex(s[:i], "-")
	if j <= 0 {
		return rpmName
	}
	return s[:j]
}

// joinVersion concatenates epoch:version-release (design 15.10). An empty ver
// yields "" (dirty data marker); a missing epoch defaults to 0.
func joinVersion(epoch, ver, rel string) string {
	if ver == "" {
		return ""
	}
	if epoch == "" {
		epoch = "0"
	}
	if rel == "" {
		return epoch + ":" + ver
	}
	return epoch + ":" + ver + "-" + rel
}

// flagsToVersionConst maps XML requires entry flags to a VersionConst; an
// empty version or unknown flag means no version constraint.
func flagsToVersionConst(flags, version string) ebsv1.VersionConst {
	if version == "" {
		return ebsv1.VersionConst{}
	}
	switch flags {
	case OpGT:
		return ebsv1.VersionConst{GT: version}
	case OpGE:
		return ebsv1.VersionConst{GE: version}
	case OpEQ:
		return ebsv1.VersionConst{EQ: version}
	case OpLE:
		return ebsv1.VersionConst{LE: version}
	case OpLT:
		return ebsv1.VersionConst{LT: version}
	default:
		return ebsv1.VersionConst{}
	}
}

func gunzip(body []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// The decompressed stream is capped too: a small compressed payload can
	// expand far beyond maxMetadataBytes (gzip bomb).
	return readLimited(r, "decompressed metadata")
}

type repomdXML struct {
	Data []struct {
		Type     string `xml:"type,attr"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"data"`
}

type primaryXML struct {
	Packages []packageXML `xml:"package"`
}

type packageXML struct {
	Name    string `xml:"name"`
	Arch    string `xml:"arch"`
	Version struct {
		Epoch string `xml:"epoch,attr"`
		Ver   string `xml:"ver,attr"`
		Rel   string `xml:"rel,attr"`
	} `xml:"version"`
	Format struct {
		SourceRpm string        `xml:"sourcerpm"`
		Provides  []rpmEntryXML `xml:"provides>entry"`
		Requires  []rpmEntryXML `xml:"requires>entry"`
	} `xml:"format"`
}

type rpmEntryXML struct {
	Name  string `xml:"name,attr"`
	Flags string `xml:"flags,attr"`
	Epoch string `xml:"epoch,attr"`
	Ver   string `xml:"ver,attr"`
	Rel   string `xml:"rel,attr"`
}
