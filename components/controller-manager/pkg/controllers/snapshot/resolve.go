package snapshot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

type resolveState string

const (
	stateResolved resolveState = "Resolved"
	stateWaiting  resolveState = "Waiting"
	stateFailed   resolveState = "Failed"
	stateSkipped  resolveState = "Skipped"
	stateFatal    resolveState = "Fatal"
)

type resolveTask struct {
	index int
	repo  ebsv1.PackageRepo
}

type resolveResult struct {
	index  int
	repo   ebsv1.PackageRepo
	state  resolveState
	status ebsv1.PackageRepoStatus
	fatal  error
}

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c *Controller) resolveAll(parent context.Context, snapshot *ebsv1.Snapshot, targets []ebsv1.PackageRepo) ([]resolveResult, error) {
	results := make([]resolveResult, 0, len(targets))
	nameCount := make(map[string]int)
	for _, repo := range targets {
		nameCount[repo.Name]++
	}
	tasks := make([]resolveTask, 0, len(targets))
	for index, repo := range targets {
		repo = effectiveRepo(repo, snapshot.Spec.DefaultRef)
		if nameCount[repo.Name] > 1 {
			results = append(results, skippedResult(index, repo, ebsv1.SpecCommitValidationFailed, "duplicate package repository name"))
			continue
		}
		if status, ok := snapshot.Status.PackageRepoStatuses[repo.Name]; ok && (status.CommitID != "" || (status.Error != nil && !status.Error.Retryable)) {
			continue
		}
		tasks = append(tasks, resolveTask{index: index, repo: repo})
	}
	if len(tasks) == 0 {
		sortResults(results)
		return results, nil
	}
	budgetCtx, cancel := context.WithTimeout(parent, c.config.ResolveBudget)
	defer cancel()
	jobs := make(chan resolveTask, len(tasks))
	resultCh := make(chan resolveResult, len(tasks))
	for _, task := range tasks {
		jobs <- task
	}
	close(jobs)
	workers := c.config.ResolveWorkers
	if workers > len(tasks) {
		workers = len(tasks)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if budgetCtx.Err() != nil {
					return
				}
				select {
				case <-budgetCtx.Done():
					return
				case task, ok := <-jobs:
					if !ok {
						return
					}
					resultCh <- c.resolveOne(budgetCtx, task, snapshot.CreationTimestamp.Time)
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(resultCh)
		close(done)
	}()
	received := make(map[int]struct{}, len(tasks))
	for result := range resultCh {
		results = append(results, result)
		received[result.index] = struct{}{}
	}
	<-done
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	for _, task := range tasks {
		if _, ok := received[task.index]; !ok {
			results = append(results, failedResult(task.index, task.repo, ebsv1.SpecCommitSyncTimeout, "snapshot resolve budget exhausted"))
		}
	}
	sortResults(results)
	for _, result := range results {
		if result.state == stateFatal {
			return nil, result.fatal
		}
	}
	return results, nil
}

func (c *Controller) resolveOne(ctx context.Context, task resolveTask, baseline time.Time) resolveResult {
	repo := task.repo
	if repo.Ref.Type == "" || repo.Ref.Value == "" {
		return skippedResult(task.index, repo, ebsv1.SpecCommitValidationFailed, "repository ref and snapshot defaultRef do not provide a complete ref")
	}
	if repo.Ref.Type == ebsv1.GitRefCommit {
		if !fullCommitPattern.MatchString(repo.Ref.Value) {
			return skippedResult(task.index, repo, ebsv1.SpecCommitValidationFailed, "commit must be a full lowercase SHA")
		}
		return resolveResult{index: task.index, repo: repo, state: stateResolved, status: ebsv1.PackageRepoStatus{CommitID: repo.Ref.Value}}
	}
	if err := c.gitClient.PublishSyncTask(ctx, repo.URL); err != nil {
		return c.gitFailure(task, "sync", err)
	}
	delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	temporaryFailures := 0
	for attempt := 0; attempt < 8; attempt++ {
		check, err := c.gitClient.CheckSynced(ctx, repo.URL, baseline)
		if err != nil {
			kind, _ := gitErrorKind(err)
			if kind == gitserver.ErrorValidation {
				return skippedResult(task.index, repo, ebsv1.SpecCommitValidationFailed, "git-server rejected repository input")
			}
			if kind == gitserver.ErrorPermanent {
				return resolveResult{index: task.index, repo: repo, state: stateFatal, fatal: controllerPermanent(err)}
			}
			temporaryFailures++
			if temporaryFailures >= 5 {
				return failedResult(task.index, repo, ebsv1.SpecCommitSyncFailed, "git-server synchronization check failed")
			}
		} else if check.Synced {
			commit, resolveErr := c.gitClient.ResolveCommit(ctx, repo.URL, repo.Ref)
			if resolveErr != nil {
				result := c.gitFailure(task, "resolve", resolveErr)
				result.status.CloneURL = check.CloneURL
				return result
			}
			return resolveResult{index: task.index, repo: repo, state: stateResolved, status: ebsv1.PackageRepoStatus{CloneURL: check.CloneURL, CommitID: commit}}
		} else {
			temporaryFailures = 0
		}
		if attempt < len(delays) {
			select {
			case <-ctx.Done():
				if err == nil {
					return resolveResult{index: task.index, repo: repo, state: stateWaiting}
				}
				return failedResult(task.index, repo, ebsv1.SpecCommitSyncTimeout, "git-server synchronization check timed out")
			case <-c.clock.After(delays[attempt]):
			}
		}
	}
	return resolveResult{index: task.index, repo: repo, state: stateWaiting}
}

func (c *Controller) gitFailure(task resolveTask, operation string, err error) resolveResult {
	if errors.Is(err, context.Canceled) && err != context.DeadlineExceeded {
		return resolveResult{index: task.index, repo: task.repo, state: stateFatal, fatal: err}
	}
	kind, _ := gitErrorKind(err)
	switch kind {
	case gitserver.ErrorValidation:
		return skippedResult(task.index, task.repo, ebsv1.SpecCommitValidationFailed, "git-server rejected repository input")
	case gitserver.ErrorNotFound:
		return skippedResult(task.index, task.repo, ebsv1.SpecCommitResolveFailed, "branch or tag was not found")
	case gitserver.ErrorPermanent:
		return resolveResult{index: task.index, repo: task.repo, state: stateFatal, fatal: controllerPermanent(err)}
	default:
		if errors.Is(err, context.DeadlineExceeded) {
			return failedResult(task.index, task.repo, ebsv1.SpecCommitSyncTimeout, operation+" request timed out")
		}
		code := ebsv1.SpecCommitSyncFailed
		if operation == "resolve" {
			code = ebsv1.SpecCommitResolveFailed
		}
		return failedResult(task.index, task.repo, code, operation+" request failed")
	}
}

func (c *Controller) mergeResults(snapshot *ebsv1.Snapshot, results []resolveResult) ([]trackerChange, bool, bool) {
	if snapshot.Status.PackageRepoStatuses == nil {
		snapshot.Status.PackageRepoStatuses = make(map[string]ebsv1.PackageRepoStatus)
	}
	accepted := make(map[string]string)
	for _, repo := range snapshot.Spec.PackageRepos {
		if status := snapshot.Status.PackageRepoStatuses[repo.Name]; status.CommitID != "" {
			accepted[repoIdentity(effectiveRepo(repo, snapshot.Spec.DefaultRef))] = status.CommitID
		}
	}
	changes := make([]trackerChange, 0, len(results))
	waiting, failed := false, false
	for _, result := range results {
		name := result.repo.Name
		switch result.state {
		case stateResolved:
			identity := repoIdentity(result.repo)
			if commit, ok := accepted[identity]; ok && commit != result.status.CommitID {
				result = skippedResult(result.index, result.repo, ebsv1.SpecCommitCommitConflict, "repository ref resolved to conflicting commits")
			} else {
				accepted[identity] = result.status.CommitID
				snapshot.Status.PackageRepoStatuses[name] = result.status
				changes = append(changes, trackerChange{repo: name})
				continue
			}
			fallthrough
		case stateSkipped:
			snapshot.Status.PackageRepoStatuses[name] = result.status
			changes = append(changes, trackerChange{repo: name})
		case stateWaiting:
			waiting = true
			if old, ok := snapshot.Status.PackageRepoStatuses[name]; ok && old.Error != nil && old.Error.Retryable {
				delete(snapshot.Status.PackageRepoStatuses, name)
			}
			changes = append(changes, trackerChange{repo: name})
		case stateFailed:
			next := c.failures.count(snapshot.UID, name) + 1
			if next >= c.config.FailureLimit {
				result.status.Error = &ebsv1.SpecCommitError{Code: ebsv1.SpecCommitRetryExhausted, Message: "temporary git-server failures exhausted retry budget", Retryable: false}
				changes = append(changes, trackerChange{repo: name})
			} else {
				failed = true
				changes = append(changes, trackerChange{repo: name, count: next})
			}
			snapshot.Status.PackageRepoStatuses[name] = result.status
		}
	}
	return changes, waiting, failed
}

func skippedResult(index int, repo ebsv1.PackageRepo, code ebsv1.SpecCommitErrorCode, message string) resolveResult {
	return resolveResult{index: index, repo: repo, state: stateSkipped, status: ebsv1.PackageRepoStatus{Error: &ebsv1.SpecCommitError{Code: code, Message: sanitize(message), Retryable: false}}}
}

func failedResult(index int, repo ebsv1.PackageRepo, code ebsv1.SpecCommitErrorCode, message string) resolveResult {
	return resolveResult{index: index, repo: repo, state: stateFailed, status: ebsv1.PackageRepoStatus{Error: &ebsv1.SpecCommitError{Code: code, Message: sanitize(message), Retryable: true}}}
}

func sortResults(results []resolveResult) {
	sort.Slice(results, func(i, j int) bool { return results[i].index < results[j].index })
}

func repoIdentity(repo ebsv1.PackageRepo) string {
	return repo.URL + "\x00" + string(repo.Ref.Type) + "\x00" + repo.Ref.Value
}

// effectiveRepo resolves the frozen Snapshot default without changing spec inputs.
func effectiveRepo(repo ebsv1.PackageRepo, defaultRef ebsv1.GitRef) ebsv1.PackageRepo {
	if repo.Ref == (ebsv1.GitRef{}) {
		repo.Ref = defaultRef
	}
	return repo
}

func controllerPermanent(err error) error {
	return controller.NewPermanentError(fmt.Errorf("git-server permanent error: %w", err))
}
