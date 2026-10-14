package job

import "sync"

type runnerIndex struct {
	mu       sync.RWMutex
	byRunner map[string]map[string]struct{}
	byJob    map[string]string
}

func newRunnerIndex() *runnerIndex {
	return &runnerIndex{byRunner: make(map[string]map[string]struct{}), byJob: make(map[string]string)}
}

func (i *runnerIndex) set(jobKey, runner string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.removeLocked(jobKey)
	if runner == "" {
		return
	}
	jobs := i.byRunner[runner]
	if jobs == nil {
		jobs = make(map[string]struct{})
		i.byRunner[runner] = jobs
	}
	jobs[jobKey] = struct{}{}
	i.byJob[jobKey] = runner
}

func (i *runnerIndex) remove(jobKey string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.removeLocked(jobKey)
}

func (i *runnerIndex) removeLocked(jobKey string) {
	runner := i.byJob[jobKey]
	if runner == "" {
		return
	}
	delete(i.byJob, jobKey)
	delete(i.byRunner[runner], jobKey)
	if len(i.byRunner[runner]) == 0 {
		delete(i.byRunner, runner)
	}
}

func (i *runnerIndex) jobs(runner string) []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	result := make([]string, 0, len(i.byRunner[runner]))
	for key := range i.byRunner[runner] {
		result = append(result, key)
	}
	return result
}
