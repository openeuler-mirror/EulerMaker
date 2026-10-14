package artifact

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (m *releaseManager) retentionLoop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.cleanupHistory(time.Now().UTC())
		}
	}
}

func (m *releaseManager) cleanupHistory(now time.Time) {
	type target struct{ project, os, arch string }
	m.mu.Lock()
	groups := map[target][]*ReleaseRecord{}
	for _, record := range m.records {
		if record.State == ReleaseReady {
			key := target{record.Project, record.TargetOS, record.TargetArch}
			groups[key] = append(groups[key], record)
		}
	}
	var remove []string
	for key, records := range groups {
		sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt.After(records[j].UpdatedAt) })
		currentTarget, _ := os.Readlink(filepath.Join(m.root, "repositories", key.project, key.os, key.arch, "current"))
		for i, record := range records {
			if i < m.historyCount || currentTarget == filepath.ToSlash(filepath.Join("releases", record.BuildName)) || now.Sub(record.UpdatedAt) < m.historyTTL {
				continue
			}
			record.State, record.UpdatedAt = ReleaseDeleting, now
			if m.persist(record) == nil {
				remove = append(remove, record.BuildName)
			}
		}
	}
	m.mu.Unlock()
	for _, name := range remove {
		go m.remove(name)
	}
}
