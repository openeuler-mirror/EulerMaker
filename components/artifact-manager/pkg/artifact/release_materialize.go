package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type filesystemReleaseMaterializer struct {
	root, createRepoCommand, publicKey string
	createRepoWorkers                  int
}

func newFilesystemReleaseMaterializer(c Config) releaseMaterializer {
	return &filesystemReleaseMaterializer{root: c.DataDir, createRepoCommand: c.CreateRepoCommand, createRepoWorkers: c.CreateRepoWorkers, publicKey: c.ReleasePublicKey}
}

func (m *filesystemReleaseMaterializer) Create(ctx context.Context, record ReleaseRecord, source RepositoryRecord) (result releaseResult, resultErr error) {
	work, err := os.MkdirTemp(filepath.Join(m.root, ".release-work"), record.BuildName+"-")
	if err != nil {
		return result, retryableReleaseError(err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	packages := filepath.Join(work, "Packages")
	if err := os.Mkdir(packages, 0750); err != nil {
		return result, retryableReleaseError(err)
	}
	excluded := make(map[string]bool, len(record.ExcludeSpecs))
	for _, spec := range record.ExcludeSpecs {
		excluded[spec] = true
	}
	for _, meta := range source.RPMs {
		if excluded[meta.SpecName] {
			continue
		}
		if meta.FileName == "" || filepath.Base(meta.FileName) != meta.FileName || filepath.Ext(meta.FileName) != ".rpm" {
			return result, &releaseError{code: "SourceRepositoryInvalid", status: 422}
		}
		from := filepath.Join(repositoryVersionPath(m.root, source.Project, source.TargetOS, source.TargetArch, source.BuildName, source.RepositoryUID), "Packages", meta.FileName)
		to := filepath.Join(packages, meta.FileName)
		if err := os.Link(from, to); err != nil {
			return result, classifyReleaseLinkError(err)
		}
	}
	command := exec.CommandContext(ctx, m.createRepoCommand, "--workers", strconv.Itoa(m.createRepoWorkers), work)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	if output, err := command.CombinedOutput(); err != nil {
		_ = output
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			return result, &releaseError{code: "ReleaseCommandTimeout", retryable: true}
		}
		return result, &releaseError{code: "ReleaseCommandFailed", retryable: true}
	}
	if err := validateReleaseMetadata(work); err != nil {
		return result, &releaseError{code: "ReleaseMetadataInvalid", retryable: true}
	}
	publicKeyDigest := ""
	if m.publicKey != "" {
		publicKeyDigest, err = copyPublicKey(m.publicKey, filepath.Join(work, "RPM-GPG-KEY-openEuler"))
		if err != nil {
			return result, retryableReleaseError(err)
		}
	}
	digest, err := digestReleaseDirectory(work)
	if err != nil {
		return result, retryableReleaseError(err)
	}
	index := releaseIndex{SchemaVersion: 1, BuildName: record.BuildName, SourceRepositoryUID: record.SourceRepositoryUID, RequestDigest: record.RequestDigest, ReleaseDigest: digest, ExcludeSpecs: record.ExcludeSpecs, PublicKeySHA256: publicKeyDigest}
	if err := atomicJSON(filepath.Join(work, "release.json"), index); err != nil {
		return result, retryableReleaseError(err)
	}
	parent := filepath.Join(m.root, "repositories", record.Project, record.TargetOS, record.TargetArch, "releases")
	if err := os.MkdirAll(parent, 0750); err != nil {
		return result, retryableReleaseError(err)
	}
	final := filepath.Join(parent, record.BuildName)
	if err := os.Rename(work, final); err != nil {
		if _, statErr := os.Stat(final); statErr == nil {
			return result, &releaseError{code: "ReleaseIdentityConflict", status: 409}
		}
		return result, retryableReleaseError(err)
	}
	if dir, err := os.Open(parent); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return releaseResult{Digest: digest}, nil
}

type repoMetadata struct {
	Records []struct {
		Checksum struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"data"`
}

func validateReleaseMetadata(root string) error {
	repomd := filepath.Join(root, "repodata", "repomd.xml")
	data, err := os.ReadFile(repomd)
	if err != nil {
		return err
	}
	var metadata repoMetadata
	if err := xml.Unmarshal(data, &metadata); err != nil || len(metadata.Records) == 0 {
		return errors.New("invalid repomd.xml")
	}
	for _, record := range metadata.Records {
		relative, err := safeRelative(record.Location.Href)
		if err != nil || !strings.HasPrefix(relative, "repodata/") || record.Checksum.Type != "sha256" || !validHash(strings.TrimSpace(record.Checksum.Value)) {
			return errors.New("invalid repository metadata reference")
		}
		actual, err := fileSHA256(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil || actual != strings.TrimSpace(record.Checksum.Value) {
			return errors.New("repository metadata checksum mismatch")
		}
	}
	return nil
}

func copyPublicKey(source, destination string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("invalid public key")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if syncErr != nil {
		return "", syncErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func digestReleaseDirectory(root string) (string, error) {
	var files []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symbolic links are not allowed")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("invalid release file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative != "release.json" {
			files = append(files, relative)
		}
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, relative := range files {
		value, err := fileSHA256(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(relative))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func retryableReleaseError(err error) error {
	if err == nil {
		return nil
	}
	return &releaseError{code: "ReleaseStorageUnavailable", retryable: true, status: 503}
}

func classifyReleaseLinkError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return &releaseError{code: "SourceRepositoryInvalid", status: 422}
	}
	return retryableReleaseError(err)
}
