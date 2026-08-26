package fsadapter

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type FS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	// WriteExecutable writes an executable file. The store's blobs are
	// written this way: immutable once in place.
	WriteExecutable(path string, data []byte) error
	MkdirAll(path string) error
	Exists(path string) (bool, error)
	IsDir(path string) (bool, error)
	List(dir string) ([]string, error)
	Remove(path string) error
	// Rename moves a file atomically; the store stages into tmp and renames
	// into the blob tree so a half-written blob is never visible.
	Rename(old, new string) error
	// Symlink links newname to target, replacing an existing link.
	Symlink(target, link string) error
}

type OS struct{}

var _ FS = OS{}

func New() OS {
	return OS{}
}

func (OS) ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return data, nil
}

func (OS) WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

func (OS) WriteExecutable(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}

	if err := os.WriteFile(path, data, 0o555); err != nil { //nolint:gosec // executables are meant to execute
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

func (OS) Rename(oldPath, newPath string) error {
	if err := os.MkdirAll(filepath.Dir(newPath), 0o750); err != nil {
		return fmt.Errorf("creating directory for %s: %w", newPath, err)
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("moving %s to %s: %w", oldPath, newPath, err)
	}

	return nil
}

func (OS) Symlink(target, link string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		return fmt.Errorf("creating directory for %s: %w", link, err)
	}

	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replacing %s: %w", link, err)
	}

	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("linking %s to %s: %w", link, target, err)
	}

	return nil
}

func (OS) MkdirAll(path string) error {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}

	return nil
}

func (OS) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}

	if os.IsNotExist(err) {
		return false, nil
	}

	return false, fmt.Errorf("inspecting %s: %w", path, err)
}

func (OS) IsDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		return info.IsDir(), nil
	}

	if os.IsNotExist(err) {
		return false, nil
	}

	return false, fmt.Errorf("inspecting %s: %w", path, err)
}

func (OS) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	sort.Strings(names)

	return names, nil
}

func (OS) Remove(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing %s: %w", path, err)
	}

	return nil
}
