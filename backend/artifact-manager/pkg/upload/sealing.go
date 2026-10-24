package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"artifact-manager/pkg/signing"
)

// SealError is a stable protocol error; command output and local paths are never exposed.
type SealError struct {
	Code      string
	Status    int
	Retryable bool
}

func (e *SealError) Error() string { return e.Code }

type Signer interface {
	Fingerprint() string
	Sign(context.Context, string) error
	Verify(context.Context, string) error
}

type signingTask struct {
	ctx  context.Context
	run  func(context.Context) error
	done chan<- error
}

// Sealer shares a bounded signing pool across all Jobs. Configure it before serving requests.
type Sealer struct {
	config    Config
	signer    Signer
	query     string
	workers   int
	timeout   time.Duration
	ctx       context.Context
	cancel    context.CancelFunc
	tasks     chan signingTask
	mu        sync.Mutex
	closed    bool
	active    sync.WaitGroup
	pool      sync.WaitGroup
	closeOnce sync.Once
}

func NewSealer(c Config, signer Signer, query string, workers int, timeout time.Duration) (*Sealer, error) {
	if workers <= 0 || timeout <= 0 || c.MaxFileSize <= 0 || c.MaxJobSize <= 0 || query == "" {
		return nil, errors.New("invalid manifest signing configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sealer{
		config:  c,
		signer:  signer,
		query:   query,
		workers: workers,
		timeout: timeout,
		ctx:     ctx,
		cancel:  cancel,
		tasks:   make(chan signingTask, 100),
	}
	for i := 0; i < workers; i++ {
		s.pool.Add(1)
		go func() {
			defer s.pool.Done()
			for task := range s.tasks {
				ctx, cancel := context.WithTimeout(task.ctx, s.timeout)
				err := ctx.Err()
				if err == nil {
					err = task.run(ctx)
				}
				cancel()
				task.done <- err
			}
		}()
	}
	return s, nil
}

func (s *Store) SetSealer(sealer *Sealer) { s.sealer = sealer }

func (s *Sealer) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancel()
		s.mu.Unlock()
		s.active.Wait()
		close(s.tasks)
		s.pool.Wait()
	})
}

func (s *Sealer) fingerprint() string {
	if s == nil || s.signer == nil {
		return ""
	}
	return s.signer.Fingerprint()
}

// run drains admitted work before returning, including on cancellation, so a retry cannot race old workers.
func (s *Sealer) run(ctx context.Context, tasks []func(context.Context) error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return &SealError{"SigningBackendUnavailable", 503, true}
	}
	s.active.Add(1)
	s.mu.Unlock()
	defer s.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	done := make(chan error, s.workers)
	next, pending := 0, 0
	var first error
	for next < len(tasks) || pending > 0 {
		if first == nil && ctx.Err() != nil {
			first = ctx.Err()
		}
		if first == nil && next < len(tasks) && pending < s.workers {
			select {
			case s.tasks <- signingTask{ctx: ctx, run: tasks[next], done: done}:
				next++
				pending++
				continue
			default:
				if pending == 0 {
					return &SealError{"SigningBusy", 503, true}
				}
			}
		}
		if pending == 0 {
			break
		}
		if err := <-done; err != nil && first == nil {
			first = err
			cancel()
		}
		pending--
	}
	if first != nil {
		var typed *SealError
		if errors.As(first, &typed) {
			return typed
		}
		return &SealError{"SigningBackendUnavailable", 503, true}
	}
	return nil
}

func isRPM(f ManifestFile) bool {
	return f.Category == CategoryArtifact && strings.HasPrefix(f.RelativePath, "packages/") &&
		strings.HasSuffix(strings.ToLower(f.RelativePath), ".rpm")
}

func signedStorageKey(project, job, id string) string {
	return filepath.ToSlash(filepath.Join(".signed", project, job, id+".rpm"))
}

// rpmIdentity includes the payload digest as well as the package identity and dependency headers.
func (s *Sealer) rpmIdentity(ctx context.Context, path string) (string, error) {
	query := `%{NAME}\t%{EPOCHNUM}\t%{VERSION}\t%{RELEASE}\t%{ARCH}\t%{SOURCERPM}\t%{PAYLOADDIGEST}`
	var parts []string
	for _, args := range [][]string{{"-qp", "--qf", query, path}, {"-qp", "--provides", path}, {"-qp", "--requires", path}} {
		cmd := exec.CommandContext(ctx, s.query, args...)
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return "", &SealError{"SigningBackendUnavailable", 503, true}
			}
			return "", &SealError{"PackageMetadataInvalid", 422, false}
		}
		value := strings.TrimSpace(string(out))
		if len(parts) == 0 {
			fields := strings.Split(value, "\t")
			if len(fields) != 7 || fields[0] == "" || fields[2] == "" || fields[3] == "" || fields[4] == "" {
				return "", &SealError{"PackageMetadataInvalid", 422, false}
			}
		} else {
			lines := strings.Split(value, "\n")
			sort.Strings(lines)
			value = strings.Join(lines, "\n")
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, "\x00"), nil
}

func (s *Sealer) prepareRPM(ctx context.Context, root, project, job string, source Artifact, f ManifestFile) (ManifestFile, error) {
	raw := filepath.Join(root, source.StorageKey)
	if err := verifySealFile(ctx, raw, f, "ArtifactMismatch"); err != nil {
		return f, err
	}
	identity, err := s.rpmIdentity(ctx, raw)
	if err != nil {
		return f, err
	}
	final := filepath.Join(root, signedStorageKey(project, job, f.ArtifactID))
	if info, err := os.Lstat(final); err == nil {
		if !info.Mode().IsRegular() {
			return f, &SealError{"SignedPackageVerificationFailed", 422, false}
		}
		return s.inspectSigned(ctx, final, identity, f)
	} else if !os.IsNotExist(err) {
		return f, &SealError{"StorageError", 503, true}
	}
	work := filepath.Join(root, ".signing", project, job)
	if err := os.MkdirAll(work, 0750); err != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	tmp, err := os.CreateTemp(work, f.ArtifactID+"-*.rpm")
	if err != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	defer os.Remove(tmp.Name())
	in, err := os.Open(raw)
	if err != nil {
		tmp.Close()
		return f, &SealError{"StorageError", 503, true}
	}
	_, copyErr := io.Copy(tmp, &contextReader{ctx: ctx, Reader: in})
	in.Close()
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	if err := s.signer.Sign(ctx, tmp.Name()); err != nil {
		if errors.Is(err, signing.ErrVerification) {
			return f, &SealError{"SignedPackageVerificationFailed", 422, false}
		}
		return f, &SealError{"SigningBackendUnavailable", 503, true}
	}
	result, err := s.inspectSigned(ctx, tmp.Name(), identity, f)
	if err != nil {
		return f, err
	}
	file, err := os.OpenFile(tmp.Name(), os.O_RDWR, 0)
	if err != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	err = file.Sync()
	closeErr = file.Close()
	if err != nil || closeErr != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	if err := os.MkdirAll(filepath.Dir(final), 0750); err != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	// Link is no-replace and the private temporary inode never belongs to an uploaded artifact.
	if err := os.Link(tmp.Name(), final); err != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	dir, err := os.Open(filepath.Dir(final))
	if err != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	return result, nil
}

func (s *Sealer) inspectSigned(ctx context.Context, path, identity string, f ManifestFile) (ManifestFile, error) {
	if err := s.signer.Verify(ctx, path); err != nil {
		if ctx.Err() != nil {
			return f, &SealError{"SigningBackendUnavailable", 503, true}
		}
		return f, &SealError{"SignedPackageVerificationFailed", 422, false}
	}
	actual, err := s.rpmIdentity(ctx, path)
	if err != nil {
		return f, err
	}
	if actual != identity {
		return f, &SealError{"SignedPackageMetadataMismatch", 422, false}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return f, &SealError{"StorageError", 503, true}
	}
	if info.Size() > s.config.MaxFileSize {
		return f, &SealError{"ArtifactLimitExceeded", 413, false}
	}
	f.Size = info.Size()
	f.SHA256, _, err = sealFileDigest(ctx, path)
	if err != nil {
		return f, &SealError{"StorageError", 503, true}
	}
	return f, nil
}

// sealFileDigest keeps file verification streaming and interruptible by the request or service shutdown.
func sealFileDigest(ctx context.Context, path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	h := sha256.New()
	size, err := io.Copy(h, &contextReader{ctx: ctx, Reader: file})
	return hex.EncodeToString(h.Sum(nil)), size, err
}

func verifySealFile(ctx context.Context, path string, file ManifestFile, mismatchCode string) error {
	sum, size, err := sealFileDigest(ctx, path)
	if ctx.Err() != nil {
		return &SealError{"SigningBackendUnavailable", 503, true}
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && !os.IsNotExist(err) {
		return &SealError{"StorageError", 503, true}
	}
	if err != nil || size != file.Size || sum != file.SHA256 {
		return &SealError{mismatchCode, 422, false}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func validateScope(project, job string) error {
	for _, part := range []string{project, job} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			return fmt.Errorf("%w: invalid Job scope", ErrInvalidManifest)
		}
	}
	return nil
}
