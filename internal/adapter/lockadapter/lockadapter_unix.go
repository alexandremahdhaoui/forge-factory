//go:build !windows

package lockadapter

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Flock locks through flock(2) on a sibling ".lock" file: advisory,
// blocking, released by the kernel even when the process dies.
type Flock struct{}

var _ Locker = (*Flock)(nil)

func New() *Flock { return &Flock{} }

func (f *Flock) Lock(path string) (func(), error) {
	lockPath := path + ".lock"

	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return nil, fmt.Errorf("preparing the lock for %s: %w", path, err)
	}

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the lock for %s: %w", path, err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("locking %s: %w", path, err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
