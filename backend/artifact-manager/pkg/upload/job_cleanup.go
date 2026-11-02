package upload

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// DeleteJob removes a Job's files and transient state. The caller must stop writers and dependent work first.
func (s *Store) DeleteJob(project, job string) error {
	for _, part := range []string{project, job} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			return errors.New("invalid job location")
		}
	}
	if err := s.removeJobDirectory(project, job); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := logKey(project, job, "combined")
	for ch := range s.subscribers[key] {
		close(ch)
	}
	delete(s.subscribers, key)
	delete(s.logRecovery, key)
	delete(s.sealing, logKey(project, job, ""))
	return nil
}

func (s *Store) removeJobDirectory(project, job string) error {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, part := range []string{"artifacts", project, "jobs"} {
		info, err := root.Lstat(part)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("job parent %q is not a directory", part)
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			return err
		}
		defer next.Close()
		opened, err := next.Stat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(info, opened) {
			return fmt.Errorf("job parent %q changed while opening", part)
		}
		root = next
	}
	// Root-relative removal does not follow the Job entry or descendant symlinks.
	if err := root.RemoveAll(job); err != nil {
		return err
	}
	parent, err := root.Open(".")
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
