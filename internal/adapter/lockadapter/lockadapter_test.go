package lockadapter_test

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/lockadapter"
)

// A second acquisition of the same key waits for the first release: that
// wait is the whole point - it is what turns two racing processes into a
// queue.
func TestASecondLockWaitsForTheFirstRelease(t *testing.T) {
	t.Parallel()

	key := filepath.Join(t.TempDir(), "git", "mirror")

	release, err := lockadapter.New().Lock(key)
	require.NoError(t, err)

	var entered atomic.Bool

	done := make(chan struct{})

	go func() {
		defer close(done)

		second, err := lockadapter.New().Lock(key)
		if err != nil {
			return
		}

		entered.Store(true)

		second()
	}()

	time.Sleep(50 * time.Millisecond)
	require.False(t, entered.Load(), "the second lock must wait while the first is held")

	release()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second lock never entered after the release")
	}

	require.True(t, entered.Load())
}

func TestDifferentKeysNeverWaitOnEachOther(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	releaseA, err := lockadapter.New().Lock(filepath.Join(dir, "a"))
	require.NoError(t, err)
	defer releaseA()

	releaseB, err := lockadapter.New().Lock(filepath.Join(dir, "b"))
	require.NoError(t, err)
	defer releaseB()
}
