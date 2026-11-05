package fileutil

import (
	"fmt"
	"os"
	"strings"
)

// OpenDirectory opens existing child directories without following symlinks.
// A missing child returns nil, nil; a missing data root is always an error.
func OpenDirectory(dataDir string, parts ...string) (*os.Root, error) {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			root.Close()
			return nil, fmt.Errorf("invalid directory component %q", part)
		}
		info, err := root.Lstat(part)
		if os.IsNotExist(err) {
			root.Close()
			return nil, nil
		}
		if err != nil || !info.IsDir() {
			root.Close()
			return nil, fmt.Errorf("cannot open directory %q: %v", part, err)
		}
		next, err := root.OpenRoot(part)
		root.Close()
		if err != nil {
			return nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			return nil, fmt.Errorf("directory %q changed while opening", part)
		}
		root = next
	}
	return root, nil
}

// RemoveEntry removes one entry beneath a verified directory and syncs its parent.
// Recursive removal unlinks descendant symlinks without following their targets.
func RemoveEntry(root *os.Root, name string, recursive bool) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("invalid entry %q", name)
	}
	var err error
	if recursive {
		err = root.RemoveAll(name)
	} else {
		err = root.Remove(name)
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	parent, err := root.Open(".")
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
