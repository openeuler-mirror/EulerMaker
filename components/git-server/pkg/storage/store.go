package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var temporaryName = regexp.MustCompile(`^(clone|delete|auth)-[0-9a-f]{32}$`)

var ErrConflict = errors.New("repository path conflicts with an existing object")

type Store struct {
	dataDir string
	dataFD  int
	tmpFD   int
	active  map[string]struct{}
	mu      sync.Mutex
}

func Open(dataDir string) (*Store, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(abs, 0750); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	dataFD, err := unix.Open(abs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open data directory: %w", err)
	}
	s := &Store{dataDir: abs, dataFD: dataFD, tmpFD: -1, active: map[string]struct{}{}}
	if err := unix.Mkdirat(dataFD, ".tmp", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		s.Close()
		return nil, fmt.Errorf("create temporary directory: %w", err)
	}
	tmpFD, err := openDirectory(dataFD, ".tmp")
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("open temporary directory: %w", err)
	}
	s.tmpFD = tmpFD
	if err := s.CleanupOrphans(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.tmpFD >= 0 {
		_ = unix.Close(s.tmpFD)
		s.tmpFD = -1
	}
	if s.dataFD >= 0 {
		_ = unix.Close(s.dataFD)
		s.dataFD = -1
	}
	return nil
}

func (s *Store) DataDir() string { return s.dataDir }

func openDirectory(parentFD int, name string) (int, error) {
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	return unix.Openat2(parentFD, name, how)
}

func splitKey(key string) ([]string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return nil, fmt.Errorf("invalid repository key")
	}
	parts := strings.Split(key, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return nil, fmt.Errorf("invalid repository key")
		}
	}
	return parts, nil
}

func (s *Store) ensureParent(key string) (int, string, error) {
	parent, name, _, err := s.parent(key, true)
	return parent, name, err
}

func (s *Store) parent(key string, create bool) (int, string, bool, error) {
	parts, err := splitKey(key)
	if err != nil {
		return -1, "", false, err
	}
	current, err := unix.Dup(s.dataFD)
	if err != nil {
		return -1, "", false, err
	}
	for _, part := range parts[:len(parts)-1] {
		if create {
			if err := unix.Mkdirat(current, part, 0750); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(current)
				return -1, "", false, fmt.Errorf("create repository parent: %w", err)
			}
		}
		next, err := openDirectory(current, part)
		unix.Close(current)
		if errors.Is(err, unix.ENOENT) && !create {
			return -1, parts[len(parts)-1], false, nil
		}
		if err != nil {
			return -1, "", false, fmt.Errorf("open repository parent: %w: %w", ErrConflict, err)
		}
		current = next
	}
	return current, parts[len(parts)-1], true, nil
}

func (s *Store) openRepository(key string) (int, int, string, error) {
	parent, name, parentExists, err := s.parent(key, false)
	if err != nil {
		return -1, -1, "", err
	}
	if !parentExists {
		return -1, -1, name, nil
	}
	fd, err := openDirectory(parent, name)
	if errors.Is(err, unix.ENOENT) {
		return parent, -1, name, nil
	}
	if err != nil {
		unix.Close(parent)
		return -1, -1, "", fmt.Errorf("open repository: %w: %w", ErrConflict, err)
	}
	return parent, fd, name, nil
}

func (s *Store) RepositoryPath(key string) (string, bool, error) {
	parent, fd, _, err := s.openRepository(key)
	if parent >= 0 {
		defer unix.Close(parent)
	}
	if err != nil {
		return "", false, err
	}
	if fd < 0 {
		return "", false, nil
	}
	unix.Close(fd)
	return filepath.Join(s.dataDir, filepath.FromSlash(key)), true, nil
}

func (s *Store) CreateTemporary(kind, id string) (string, string, error) {
	name := kind + "-" + id
	if !temporaryName.MatchString(name) {
		return "", "", fmt.Errorf("invalid temporary directory name")
	}
	s.mu.Lock()
	if _, exists := s.active[name]; exists {
		s.mu.Unlock()
		return "", "", fs.ErrExist
	}
	s.active[name] = struct{}{}
	s.mu.Unlock()
	if err := unix.Mkdirat(s.tmpFD, name, 0700); err != nil {
		s.unmark(name)
		return "", "", err
	}
	return name, filepath.Join(s.dataDir, ".tmp", name), nil
}

func (s *Store) Publish(tempName, key string) error {
	if !temporaryName.MatchString(tempName) || !strings.HasPrefix(tempName, "clone-") {
		return fmt.Errorf("invalid clone directory")
	}
	sourceFD, err := openDirectory(s.tmpFD, tempName)
	if err != nil {
		return err
	}
	var before unix.Stat_t
	err = unix.Fstat(sourceFD, &before)
	unix.Close(sourceFD)
	if err != nil {
		return err
	}
	parent, name, err := s.ensureParent(key)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if err := unix.Renameat2(s.tmpFD, tempName, parent, name, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return ErrConflict
		}
		return err
	}
	destFD, err := openDirectory(parent, name)
	if err != nil {
		return fmt.Errorf("verify published repository: %w", err)
	}
	defer unix.Close(destFD)
	var after unix.Stat_t
	if err := unix.Fstat(destFD, &after); err != nil {
		return err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino {
		return fmt.Errorf("published repository inode changed")
	}
	if err := unix.Fsync(parent); err != nil {
		return fmt.Errorf("sync repository parent: %w", err)
	}
	s.unmark(tempName)
	return nil
}

func (s *Store) Quarantine(key, id string) (string, bool, error) {
	name := "delete-" + id
	if !temporaryName.MatchString(name) {
		return "", false, fmt.Errorf("invalid delete directory")
	}
	parent, sourceFD, repositoryName, err := s.openRepository(key)
	if parent >= 0 {
		defer unix.Close(parent)
	}
	if err != nil {
		return "", false, err
	}
	if sourceFD < 0 {
		return "", false, nil
	}
	var before unix.Stat_t
	if err := unix.Fstat(sourceFD, &before); err != nil {
		unix.Close(sourceFD)
		return "", false, err
	}
	unix.Close(sourceFD)
	s.mu.Lock()
	s.active[name] = struct{}{}
	s.mu.Unlock()
	if err := unix.Renameat2(parent, repositoryName, s.tmpFD, name, unix.RENAME_NOREPLACE); err != nil {
		s.unmark(name)
		return "", false, err
	}
	destFD, err := openDirectory(s.tmpFD, name)
	if err != nil {
		return "", false, err
	}
	var after unix.Stat_t
	err = unix.Fstat(destFD, &after)
	unix.Close(destFD)
	if err != nil || before.Dev != after.Dev || before.Ino != after.Ino {
		return "", false, fmt.Errorf("quarantined repository inode changed")
	}
	if err := unix.Fsync(parent); err != nil {
		return "", false, err
	}
	return name, true, nil
}

func (s *Store) Cleanup(name string) error {
	if !temporaryName.MatchString(name) {
		return fmt.Errorf("refusing to clean unknown temporary path")
	}
	err := removeTreeAt(s.tmpFD, name)
	s.unmark(name)
	return err
}

func (s *Store) unmark(name string) {
	s.mu.Lock()
	delete(s.active, name)
	s.mu.Unlock()
}

func (s *Store) CleanupOrphans() error {
	entries, err := readDir(s.tmpFD)
	if err != nil {
		return fmt.Errorf("scan temporary directory: %w", err)
	}
	for _, entry := range entries {
		if !temporaryName.MatchString(entry.Name()) {
			continue
		}
		s.mu.Lock()
		_, active := s.active[entry.Name()]
		s.mu.Unlock()
		if !active {
			if err := removeTreeAt(s.tmpFD, entry.Name()); err != nil {
				return fmt.Errorf("clean temporary directory %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

func removeTreeAt(parentFD int, name string) error {
	fd, err := openDirectory(parentFD, name)
	if err != nil {
		return err
	}
	entries, err := readDir(fd)
	if err != nil {
		unix.Close(fd)
		return err
	}
	for _, entry := range entries {
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			unix.Close(fd)
			return err
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := removeTreeAt(fd, entry.Name()); err != nil {
				unix.Close(fd)
				return err
			}
		case unix.S_IFLNK:
			unix.Close(fd)
			return fmt.Errorf("refusing to follow symbolic link")
		default:
			if err := unix.Unlinkat(fd, entry.Name(), 0); err != nil {
				unix.Close(fd)
				return err
			}
		}
	}
	unix.Close(fd)
	return unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
}

func readDir(fd int) ([]os.DirEntry, error) {
	duplicate, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "directory")
	defer file.Close()
	return file.ReadDir(-1)
}

func (s *Store) RepositoryKeys() ([]string, error) {
	var keys []string
	if err := walkRepositories(s.dataFD, "", &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func walkRepositories(fd int, prefix string, keys *[]string) error {
	entries, err := readDir(fd)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if prefix == "" && entry.Name() == ".tmp" {
			continue
		}
		if !entry.IsDir() {
			continue
		}
		child, err := openDirectory(fd, entry.Name())
		if err != nil {
			continue
		}
		key := entry.Name()
		if prefix != "" {
			key = prefix + "/" + entry.Name()
		}
		if strings.HasSuffix(entry.Name(), ".git") {
			*keys = append(*keys, key)
			unix.Close(child)
			continue
		}
		err = walkRepositories(child, key, keys)
		unix.Close(child)
		if err != nil {
			return err
		}
	}
	return nil
}
