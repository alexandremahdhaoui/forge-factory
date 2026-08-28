package runcontroller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// failLocker refuses every lock. A lock this controller cannot take means
// another process may be writing the same clone, so every caller must stop
// rather than proceed unserialized.
type failLocker struct{}

func (failLocker) Lock(string) (func(), error) {
	return nil, errors.New("the lock is held")
}

func (r *rig) withFailingLock() {
	r.c = New(r.fs, r.git, r.exec, r.sync, failLocker{}, r.out)
}

// A worktree registration lives inside the clone, so it is shared state
// between every process that names the same clone. Proceeding without the
// lock would let two of them register into one git directory at once.
func TestALockThatCannotBeTakenStopsTheWork(t *testing.T) {
	r := newRig(t)
	r.withFailingLock()

	err := r.c.worktreeAdd(context.Background(), t.TempDir(), "aaa", t.TempDir())
	require.ErrorContains(t, err, "the lock is held")

}

func TestWarmTupleIsBestEffort(t *testing.T) {
	// The warm marker is an optimisation. Failing to write it must never
	// fail the run it was meant to speed up, because the next run rebuilds
	// what the marker would have skipped.
	target := runnable{Name: "tool", Src: "./cmd/tool"}

	t.Run("an unwritable path", func(t *testing.T) {
		r := newRig(t)

		// A regular file where the marker's directory belongs, so MkdirAll
		// fails before anything is written.
		blocked := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(blocked, nil, 0o600))

		require.NotPanics(t, func() {
			r.c.writeWarmTuple(blocked, "some-key", "/repo", target)
		})
	})

	t.Run("a refusing filesystem", func(t *testing.T) {
		r := newRig(t)
		cache := t.TempDir()
		r.fs.writeErr[warmTuplePath(cache, "some-key")] = errors.New("read only")

		require.NotPanics(t, func() {
			r.c.writeWarmTuple(cache, "some-key", "/repo", target)
		})
	})
}
