package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const agentVersion = "v0.1.0"

type RunnerAPI interface {
	GetRunner(context.Context, string) (*RunnerResource, error)
	CreateRunner(context.Context, RunnerResource) error
	UpdateRunner(context.Context, RunnerResource) error
	PatchRunnerStatus(context.Context, string, RunnerStatus) error
	PatchJobStatus(context.Context, string, string, JobStatus) error
	ListAssignedJobs(context.Context, string) (*JobList, error)
	WatchAssignedJobs(context.Context, string, string) (<-chan WatchEvent, <-chan error)
}

type Agent struct {
	cfg        Config
	client     RunnerAPI
	tokens     *TokenProvider
	executor   Executor
	logFactory *ArtifactLogFactory
	artifacts  *ArtifactProcessor
	cleanup    *ArtifactCleanupManager

	mu         sync.Mutex
	activeJobs map[string]struct{}
	lastRV     string
}

func NewAgent(cfg Config) (*Agent, error) {
	httpClient, err := cfg.HTTPClient()
	if err != nil {
		return nil, err
	}
	credential, err := LoadMachineCredential(cfg.MachineCredentialFile)
	if err != nil {
		return nil, err
	}
	tokens, err := NewTokenProvider(cfg.Gateway, cfg.Name, credential, httpClient)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(cfg.Gateway, tokens, httpClient)
	if err != nil {
		return nil, err
	}
	artifactHTTPClient, err := cfg.ArtifactManagerHTTPClient()
	if err != nil {
		return nil, err
	}
	artifactClient, err := NewArtifactClient(cfg.ArtifactManager, tokens, artifactHTTPClient)
	if err != nil {
		return nil, err
	}
	logFactory := &ArtifactLogFactory{
		Remote:          artifactClient,
		RootDir:         cfg.RootDir,
		ChunkSize:       cfg.LogChunkSize,
		FlushInterval:   cfg.LogFlushInterval,
		SpoolLimit:      cfg.LogSpoolLimit,
		RetryMaxBackoff: cfg.LogRetryMaxBackoff,
	}
	artifactProcessor := &ArtifactProcessor{
		Remote: artifactClient, RootDir: cfg.RootDir,
		MaxFileSize: cfg.ArtifactMaxFileSize, MaxJobSize: cfg.ArtifactMaxJobSize,
		MaxFiles: cfg.ArtifactMaxFiles, Concurrency: cfg.ArtifactUploadConcurrency,
		RetryMaxBackoff: cfg.ArtifactRetryMaxBackoff,
	}
	cleanupManager := &ArtifactCleanupManager{RootDir: cfg.RootDir, FailedRetention: cfg.ArtifactFailedRetention}
	return &Agent{
		cfg:    cfg,
		client: client,
		tokens: tokens,
		executor: &RuntimeManager{
			RunnerType: cfg.Type,
			Executors: map[string]Executor{
				"ct": &CTExecutor{
					WorkDir:         workDir(cfg.RootDir),
					ResultRoot:      resultRoot(cfg.RootDir),
					RunnerName:      cfg.Name,
					LogFactory:      logFactory,
					LogDrainTimeout: cfg.LogDrainTimeout,
				},
			},
		},
		logFactory: logFactory,
		artifacts:  artifactProcessor,
		cleanup:    cleanupManager,
		activeJobs: make(map[string]struct{}),
	}, nil
}

func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(workDir(a.cfg.RootDir), 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	if err := os.MkdirAll(resultRoot(a.cfg.RootDir), 0o755); err != nil {
		return fmt.Errorf("create result root: %w", err)
	}
	if err := a.waitForInitialToken(ctx); err != nil {
		return err
	}

	if err := a.register(ctx); err != nil {
		return err
	}
	if err := a.patchRunnerPhase(ctx, "Booting"); err != nil {
		log.Printf("update booting status failed: %v", err)
	}

	go a.heartbeatLoop(ctx)
	go a.watchLoop(ctx)
	go a.cleanup.Run(ctx)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.patchRunnerPhase(shutdownCtx, "Offline"); err != nil {
		log.Printf("update offline status failed: %v", err)
	}
	return nil
}

func (a *Agent) waitForInitialToken(ctx context.Context) error {
	backoff := time.Second
	for {
		if _, err := a.tokens.Token(ctx); err == nil {
			return nil
		} else {
			wait := backoff
			var statusErr StatusError
			if errors.As(err, &statusErr) {
				switch statusErr.Code {
				case 400, 401:
					wait = 30 * time.Second
				case 429:
					if statusErr.RetryAfter > wait {
						wait = statusErr.RetryAfter
					}
				}
			}
			log.Printf("obtain runner token failed; retrying in %s: %v", wait, err)
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return ctx.Err()
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}
	}
}

func (a *Agent) register(ctx context.Context) error {
	desired := a.runnerObject("")
	existing, err := a.client.GetRunner(ctx, a.cfg.Name)
	if err == nil {
		desired = *existing
		desired.TypeMeta = TypeMeta{APIVersion: "ebs/v1", Kind: "Runner"}
		if desired.Metadata.Labels == nil {
			desired.Metadata.Labels = map[string]string{}
		}
		desired.Metadata.Labels["ebs.io/runner-type"] = a.cfg.Type
		desired.Metadata.Labels["ebs.io/runner-arch"] = a.cfg.Arch
		desired.Spec.Type = a.cfg.Type
		desired.Spec.Arch = a.cfg.Arch
		desired.Spec.Hostname = a.cfg.Name
		if err := a.client.UpdateRunner(ctx, desired); err != nil {
			return fmt.Errorf("update runner: %w", err)
		}
		return nil
	}
	var statusErr StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != 404 {
		return fmt.Errorf("get runner: %w", err)
	}
	if err := a.client.CreateRunner(ctx, desired); err != nil {
		return fmt.Errorf("create runner: %w", err)
	}
	return nil
}

func (a *Agent) runnerObject(resourceVersion string) RunnerResource {
	return RunnerResource{
		TypeMeta: TypeMeta{APIVersion: "ebs/v1", Kind: "Runner"},
		Metadata: ObjectMeta{
			Name:            a.cfg.Name,
			ResourceVersion: resourceVersion,
			Labels: map[string]string{
				"ebs.io/runner-type": a.cfg.Type,
				"ebs.io/runner-arch": a.cfg.Arch,
			},
		},
		Spec: RunnerSpec{
			Type:     a.cfg.Type,
			Arch:     a.cfg.Arch,
			Hostname: a.cfg.Name,
		},
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()

	a.sendHeartbeat(ctx)
	for {
		select {
		case <-ticker.C:
			a.sendHeartbeat(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (a *Agent) sendHeartbeat(ctx context.Context) {
	if err := a.client.PatchRunnerStatus(ctx, a.cfg.Name, a.currentStatus()); err != nil {
		log.Printf("heartbeat failed: %v", err)
	}
}

func (a *Agent) patchRunnerPhase(ctx context.Context, phase string) error {
	status := a.currentStatus()
	status.Phase = phase
	return a.client.PatchRunnerStatus(ctx, a.cfg.Name, status)
}

func (a *Agent) currentStatus() RunnerStatus {
	now := time.Now().UTC()
	phase := "Idle"
	if a.runningJobCount() > 0 {
		phase = "Running"
	}
	capacity, allocatable := nodeResources(workDir(a.cfg.RootDir))
	return RunnerStatus{
		Phase:       phase,
		Capacity:    capacity,
		Allocatable: allocatable,
		Addresses:   runnerAddresses(a.cfg.Name),
		Info: RunnerInfo{
			OS:            runtime.GOOS,
			Arch:          a.cfg.Arch,
			AgentVersion:  agentVersion,
			KernelVersion: kernelVersion(),
		},
		Heartbeat: &now,
	}
}

func (a *Agent) watchLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		startedAt := time.Now()
		err := a.watchOnce(ctx)
		if errors.Is(err, context.Canceled) {
			return
		}
		watchDuration := time.Since(startedAt)
		if watchDuration >= 30*time.Second {
			backoff = time.Second
		}
		wait := time.Duration(0)
		if err != nil {
			wait = backoff
			log.Printf("watch jobs failed: %v", err)
			var statusErr StatusError
			if errors.As(err, &statusErr) {
				if statusErr.Code == 410 {
					a.resetLastResourceVersion()
					backoff = time.Second
					continue
				}
				if statusErr.Code == 429 && statusErr.RetryAfter > wait {
					wait = statusErr.RetryAfter
				}
			}
		} else {
			backoff = time.Second
			if watchDuration < time.Second {
				wait = backoff
			}
		}

		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
		}
		if err != nil && watchDuration < 30*time.Second && backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (a *Agent) watchOnce(ctx context.Context) error {
	if a.lastResourceVersion() == "" {
		list, err := a.client.ListAssignedJobs(ctx, a.cfg.Name)
		if err != nil {
			return err
		}
		for _, job := range list.Items {
			a.handleEvent(ctx, WatchEvent{Type: "ADDED", Object: job})
		}
		a.setLastResourceVersion(list.Metadata.ResourceVersion)
	}
	events, errs := a.client.WatchAssignedJobs(ctx, a.cfg.Name, a.lastResourceVersion())
	for {
		select {
		case event, ok := <-events:
			if !ok {
				if err := <-errs; err != nil {
					return err
				}
				return nil
			}
			a.setLastResourceVersion(event.Object.Metadata.ResourceVersion)
			if event.Type != "BOOKMARK" {
				a.handleEvent(ctx, event)
			}
		case err, ok := <-errs:
			if ok && err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *Agent) handleEvent(ctx context.Context, event WatchEvent) {
	if event.Type == "DELETED" {
		return
	}
	job := event.Object
	if job.Status.Runner != a.cfg.Name || job.Status.Phase != "Running" {
		return
	}
	key := jobKey(job)
	if !a.tryStartJob(key) {
		return
	}
	if job.Status.Stage == "PostRun" {
		go a.resumePostRun(ctx, key, job)
		return
	}
	go a.runJob(ctx, key, job)
}

func (a *Agent) runJob(parent context.Context, key string, job JobResource) {
	defer a.finishJob(key)

	now := time.Now().UTC()
	status := job.Status
	status.Stage = "Running"
	status.StartTime = &now
	if err := a.client.PatchJobStatus(parent, job.Metadata.Namespace, job.Metadata.Name, status); err != nil {
		log.Printf("update job running status failed: %v", err)
	}

	execCtx := parent
	var cancel context.CancelFunc
	if job.Spec.TimeoutSeconds > 0 {
		execCtx, cancel = context.WithTimeout(parent, time.Duration(job.Spec.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	resultRoot, executionErr := a.executor.Execute(execCtx, job)
	status.ResultRoot = resultRoot
	status.Phase = "Running"
	status.Stage = "PostRun"
	status.ArtifactState = "Uploading"
	if executionErr != nil {
		status.Message = executionErr.Error()
	} else {
		status.Message = ""
	}
	if err := a.client.PatchJobStatus(context.Background(), job.Metadata.Namespace, job.Metadata.Name, status); err != nil {
		log.Printf("update job post-run status failed: %v", err)
	}
	a.finalizeArtifacts(parent, job, status, resultRoot, executionErr)
	a.sendHeartbeat(context.Background())
}

func (a *Agent) resumePostRun(parent context.Context, key string, job JobResource) {
	defer a.finishJob(key)
	resultDir := job.Status.ResultRoot
	if resultDir == "" || strings.HasPrefix(resultDir, "artifact://") {
		resultDir = filepath.Join(resultRoot(a.cfg.RootDir), job.Metadata.Namespace, job.Metadata.UID)
	}
	var executionErr error
	if job.Status.Message != "" {
		executionErr = errors.New(job.Status.Message)
	}
	a.finalizeArtifacts(parent, job, job.Status, resultDir, executionErr)
	a.sendHeartbeat(context.Background())
}

func (a *Agent) finalizeArtifacts(parent context.Context, job JobResource, status JobStatus, resultDir string, executionErr error) {
	artifactCtx, cancelArtifacts := context.WithTimeout(parent, a.cfg.ArtifactUploadTimeout)
	manifest, artifactErr := a.artifacts.Finalize(artifactCtx, job, resultDir, executionErr == nil)
	cancelArtifacts()
	end := time.Now().UTC()
	status.EndTime = &end
	if artifactErr != nil {
		status.Phase = "Failed"
		status.Stage = "Failed"
		status.ArtifactState = "Failed"
		if executionErr != nil {
			status.Message = executionErr.Error() + "; artifact upload: " + artifactErr.Error()
		} else {
			status.Message = "artifact upload: " + artifactErr.Error()
		}
	} else {
		status.ResultRoot = "artifact://" + job.Metadata.UID
		status.ArtifactState = "Completed"
		status.ArtifactGeneration = manifest.Generation
		status.ArtifactDigest = manifest.Digest
		status.ArtifactCount = manifest.ArtifactCount
		if executionErr != nil {
			status.Phase = "Failed"
			status.Stage = "Failed"
			status.Message = executionErr.Error()
		} else {
			status.Phase = "Completed"
			status.Stage = "PostRun"
			status.Message = ""
		}
	}
	if updateErr := a.client.PatchJobStatus(context.Background(), job.Metadata.Namespace, job.Metadata.Name, status); updateErr != nil {
		log.Printf("update job final status failed: %v", updateErr)
	} else if artifactErr != nil {
		if cleanupErr := a.cleanup.MarkFailure(job); cleanupErr != nil {
			log.Printf("schedule failed artifact cleanup: %v", cleanupErr)
		}
	} else {
		if cleanupErr := a.cleanup.MarkSuccess(job); cleanupErr != nil {
			log.Printf("clean completed artifact state: %v", cleanupErr)
		}
	}
}

func (a *Agent) tryStartJob(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.activeJobs[key]; ok {
		return false
	}
	a.activeJobs[key] = struct{}{}
	return true
}

func (a *Agent) finishJob(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.activeJobs, key)
}

func (a *Agent) runningJobCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.activeJobs)
}

func (a *Agent) lastResourceVersion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastRV
}

func (a *Agent) setLastResourceVersion(rv string) {
	if rv == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastRV = rv
}

func (a *Agent) resetLastResourceVersion() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastRV = ""
}

func jobKey(job JobResource) string {
	if job.Metadata.Namespace == "" {
		return job.Metadata.Name
	}
	return job.Metadata.Namespace + "/" + job.Metadata.Name
}

func workDir(rootDir string) string {
	return filepath.Join(rootDir, "work")
}

func resultRoot(rootDir string) string {
	return filepath.Join(rootDir, "results")
}

func runnerAddresses(hostname string) []RunnerAddress {
	addresses := []RunnerAddress{{Type: "Hostname", Address: hostname}}
	if ip := firstNonLoopbackIP(); ip != "" {
		addresses = append(addresses, RunnerAddress{Type: "InternalIP", Address: ip})
	}
	return addresses
}

func nodeResources(path string) (map[string]string, map[string]string) {
	capacity := map[string]string{
		"cpu": strconv.Itoa(runtime.NumCPU()),
	}
	allocatable := map[string]string{
		"cpu": strconv.Itoa(runtime.NumCPU()),
	}
	if memory := totalMemory(); memory != "" {
		capacity["memory"] = memory
		allocatable["memory"] = memory
	}
	if storageCapacity, storageAllocatable := ephemeralStorage(path); storageCapacity != "" {
		capacity["ephemeral-storage"] = storageCapacity
		allocatable["ephemeral-storage"] = storageAllocatable
	}
	return capacity, allocatable
}

func totalMemory() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	return memoryQuantityFromMeminfo(string(data))
}

func memoryQuantityFromMeminfo(data string) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return ""
			}
			return fmt.Sprintf("%dMi", kib/1024)
		}
	}
	return ""
}

func ephemeralStorage(path string) (string, string) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", ""
	}
	capacity := bytesToGiQuantity(stat.Blocks * uint64(stat.Bsize))
	allocatable := bytesToGiQuantity(stat.Bavail * uint64(stat.Bsize))
	return capacity, allocatable
}

func bytesToGiQuantity(bytes uint64) string {
	return fmt.Sprintf("%dGi", bytes/(1024*1024*1024))
}

func firstNonLoopbackIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

func kernelVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
