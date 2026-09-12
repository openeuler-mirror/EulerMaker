package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type filesystemMaterializer struct {
	root              string
	store             *Store
	createRepoCommand string
	rpmQueryCommand   string
	createRepoWorkers int
}

func newFilesystemMaterializer(c Config, store *Store) repositoryMaterializer {
	return &filesystemMaterializer{root: c.DataDir, store: store, createRepoCommand: c.CreateRepoCommand, rpmQueryCommand: c.RPMQueryCommand, createRepoWorkers: c.CreateRepoWorkers}
}

func (m *filesystemMaterializer) Materialize(ctx context.Context, record RepositoryRecord) (result repositoryResult, resultErr error) {
	inputs, err := m.store.repositoryArtifacts(record.Project, record.Manifests)
	if err != nil {
		return result, err
	}
	work, err := os.MkdirTemp(filepath.Join(m.root, ".repository-work"), record.RepositoryUID+"-")
	if err != nil {
		return result, retryableRepositoryError(err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	packages := filepath.Join(work, "Packages")
	if err = os.Mkdir(packages, 0750); err != nil {
		return result, retryableRepositoryError(err)
	}

	metadata := make(map[string]RepositoryRPMMeta)
	if record.BaseRepositoryUID != "" {
		if record.baseBuildName == "" {
			return result, &repositoryError{code: "BaseRepositoryNotReady", status: 422}
		}
		baseRepository := repositoryVersionPath(m.root, record.Project, record.TargetArch, record.baseBuildName, record.BaseRepositoryUID)
		basePackages := filepath.Join(baseRepository, "Packages")
		if err = linkRPMDirectory(basePackages, packages); err != nil {
			return result, classifyLinkError(err)
		}
		if err = copyDirectory(filepath.Join(baseRepository, "repodata"), filepath.Join(work, "repodata")); err != nil && !os.IsNotExist(err) {
			return result, retryableRepositoryError(err)
		}
	}

	inputMetadata := make([]RepositoryRPMMeta, 0, len(inputs))
	inputSpecs := make(map[string]bool)
	for _, input := range inputs {
		meta, err := m.inspectRPM(ctx, input.Path, input.Metadata)
		if err != nil {
			return result, err
		}
		if meta.Arch != "noarch" && meta.Arch != "src" && meta.Arch != record.TargetArch {
			return result, &repositoryError{code: "PackageArchitectureMismatch", status: 422}
		}
		inputMetadata = append(inputMetadata, meta)
		inputSpecs[meta.SpecName] = true
	}

	baseEntries, err := os.ReadDir(packages)
	if err != nil {
		return result, retryableRepositoryError(err)
	}
	for _, entry := range baseEntries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return result, &repositoryError{code: "RepositoryLayoutInvalid", status: 422}
		}
		path := filepath.Join(packages, entry.Name())
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return result, &repositoryError{code: "RepositoryLayoutInvalid", status: 422}
		}
		meta, err := m.inspectRPMFile(ctx, path)
		if err != nil {
			return result, err
		}
		if inputSpecs[meta.SpecName] {
			if err := os.Remove(path); err != nil {
				return result, retryableRepositoryError(err)
			}
			continue
		}
		metadata[meta.FileName] = meta
	}

	nevra := make(map[string]string)
	for name, meta := range metadata {
		nevra[rpmIdentity(meta)] = name + "\x00" + meta.SHA256
	}
	for i, input := range inputs {
		meta := inputMetadata[i]
		if old, ok := metadata[meta.FileName]; ok && old.SHA256 != meta.SHA256 {
			return result, &repositoryError{code: "PackageConflict", status: 422}
		}
		if old, ok := nevra[rpmIdentity(meta)]; ok && !strings.HasSuffix(old, "\x00"+meta.SHA256) {
			return result, &repositoryError{code: "PackageConflict", status: 422}
		}
		destination := filepath.Join(packages, meta.FileName)
		if _, err := os.Stat(destination); os.IsNotExist(err) {
			if err := os.Link(input.Path, destination); err != nil {
				return result, classifyLinkError(err)
			}
		}
		metadata[meta.FileName] = meta
		nevra[rpmIdentity(meta)] = meta.FileName + "\x00" + meta.SHA256
	}

	command := exec.CommandContext(ctx, m.createRepoCommand, "--update", "--workers", strconv.Itoa(m.createRepoWorkers), work)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	output, err := command.CombinedOutput()
	if err != nil {
		_ = output // Command output must not leak paths through the public failure response.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			return result, &repositoryError{code: "RepositoryCommandTimeout", retryable: true}
		}
		return result, &repositoryError{code: "RepositoryCommandFailed", retryable: true}
	}
	if info, err := os.Stat(filepath.Join(work, "repodata", "repomd.xml")); err != nil || !info.Mode().IsRegular() {
		return result, &repositoryError{code: "RepositoryMetadataInvalid", retryable: true}
	}
	digest, err := digestDirectory(work)
	if err != nil {
		return result, retryableRepositoryError(err)
	}
	repositoryJSON := repositoryIndex{1, record.RepositoryUID, record.RequestDigest, digest, metadata}
	if err := atomicJSON(filepath.Join(work, "repository.json"), repositoryJSON); err != nil {
		return result, retryableRepositoryError(err)
	}
	finalParent := filepath.Join(m.root, "repositories", record.Project, record.TargetArch, "history", record.BuildName, "steps")
	if err := os.MkdirAll(finalParent, 0750); err != nil {
		return result, retryableRepositoryError(err)
	}
	final := filepath.Join(finalParent, record.RepositoryUID)
	if err := os.Rename(work, final); err != nil {
		if os.IsExist(err) {
			return result, &repositoryError{code: "RepositoryIdentityConflict", status: 409}
		}
		return result, retryableRepositoryError(err)
	}
	if dir, err := os.Open(filepath.Dir(final)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return repositoryResult{Digest: digest, Count: len(metadata), RPMs: metadata}, nil
}

func (m *filesystemMaterializer) inspectRPM(ctx context.Context, path string, artifact Artifact) (RepositoryRPMMeta, error) {
	meta, err := m.inspectRPMFile(ctx, path)
	if err != nil {
		return meta, err
	}
	meta.FileName, meta.SHA256, meta.Size = filepath.Base(artifact.RelativePath), artifact.SHA256, artifact.Size
	return meta, nil
}

func (m *filesystemMaterializer) inspectRPMFile(ctx context.Context, path string) (RepositoryRPMMeta, error) {
	const query = `%{NAME}\t%{EPOCHNUM}\t%{VERSION}\t%{RELEASE}\t%{ARCH}\t%{SOURCERPM}`
	output, err := exec.CommandContext(ctx, m.rpmQueryCommand, "-qp", "--qf", query, path).Output()
	if err != nil {
		return RepositoryRPMMeta{}, &repositoryError{code: "PackageMetadataInvalid", status: 422}
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "\t")
	if len(fields) != 6 || fields[0] == "" || fields[2] == "" || fields[3] == "" || fields[4] == "" {
		return RepositoryRPMMeta{}, &repositoryError{code: "PackageMetadataInvalid", status: 422}
	}
	info, err := os.Stat(path)
	if err != nil {
		return RepositoryRPMMeta{}, retryableRepositoryError(err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return RepositoryRPMMeta{}, retryableRepositoryError(err)
	}
	spec := fields[0]
	if fields[5] != "(none)" && fields[5] != "" {
		suffix := "-" + fields[2] + "-" + fields[3] + ".src.rpm"
		if !strings.HasSuffix(fields[5], suffix) {
			return RepositoryRPMMeta{}, &repositoryError{code: "PackageMetadataInvalid", status: 422}
		}
		spec = strings.TrimSuffix(fields[5], suffix)
	}
	if spec == "" {
		return RepositoryRPMMeta{}, &repositoryError{code: "PackageMetadataInvalid", status: 422}
	}
	provides, err := m.queryRPMList(ctx, "--provides", path)
	if err != nil {
		return RepositoryRPMMeta{}, err
	}
	requires, err := m.queryRPMList(ctx, "--requires", path)
	if err != nil {
		return RepositoryRPMMeta{}, err
	}
	return RepositoryRPMMeta{FileName: filepath.Base(path), SHA256: sum, Size: info.Size(), Name: fields[0], Epoch: fields[1], Version: fields[2], Release: fields[3], Arch: fields[4], Source: fields[5], SpecName: spec, Provides: provides, Requires: requires}, nil
}

func (m *filesystemMaterializer) queryRPMList(ctx context.Context, option, path string) ([]string, error) {
	output, err := exec.CommandContext(ctx, m.rpmQueryCommand, "-qp", option, path).Output()
	if err != nil {
		return nil, &repositoryError{code: "PackageMetadataInvalid", status: 422}
	}
	values := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(values) == 1 && values[0] == "" {
		return nil, nil
	}
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	sort.Strings(values)
	return values, nil
}

func rpmIdentity(meta RepositoryRPMMeta) string {
	return strings.Join([]string{meta.Name, meta.Epoch, meta.Version, meta.Release, meta.Arch}, "\x00")
}

func linkRPMDirectory(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(strings.ToLower(entry.Name()), ".rpm") {
			return errors.New("invalid package directory")
		}
		if err := os.Link(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(source, path)
		target := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symbolic links are not allowed")
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0750)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("invalid repository file")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func digestDirectory(root string) (string, error) {
	var files []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symbolic links are not allowed")
		}
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return errors.New("invalid repository file")
			}
			relative, _ := filepath.Rel(root, path)
			relative = filepath.ToSlash(relative)
			if relative != "repository.json" {
				files = append(files, relative)
			}
		}
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, relative := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return "", err
		}
		for _, value := range []string{relative, fmt.Sprint(info.Size()), sum} {
			var length [8]byte
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			hash.Write(length[:])
			hash.Write([]byte(value))
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func classifyLinkError(err error) error {
	if errors.Is(err, syscall.EXDEV) {
		return &repositoryError{code: "RepositoryFilesystemMismatch", status: 422}
	}
	return retryableRepositoryError(err)
}

func retryableRepositoryError(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return &repositoryError{code: "InsufficientStorage", retryable: true}
	}
	return &repositoryError{code: "RepositoryStorageUnavailable", retryable: true}
}
