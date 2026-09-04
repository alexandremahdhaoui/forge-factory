package toolingcontroller_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/lockadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/toolingcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
)

var mockery = toolingcontroller.Binary{
	Name: "mockery", Module: "github.com/vektra/mockery/v3", Version: "v3.5.5",
}

// A (module, version) is one binary per platform, and the store says which.
// A store restored from a cache, or shared between a container job and a
// host job, is read on more than one architecture; keyed by name alone it
// served whichever landed first.
func TestAProvisionedBinaryIsKeyedByItsPlatform(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	runner := execadaptermock.NewMockRunner(t)
	goInstallFake(t, runner, "built-here")

	_, err := toolingcontroller.New(fsadapter.New(), runner, lockadapter.New()).
		ProvisionBinaries(context.Background(), t.TempDir(), store, []toolingcontroller.Binary{mockery})
	require.NoError(t, err)

	entry := filepath.Join(store, "tools", "github.com-vektra-mockery-v3@v3.5.5",
		runtime.GOOS+"-"+runtime.GOARCH, "mockery")
	built, err := os.ReadFile(entry)
	require.NoError(t, err, "the entry lives under its platform")
	assert.Equal(t, "built-here", string(built))

	_, err = os.Stat(filepath.Join(store, "tools", "github.com-vektra-mockery-v3@v3.5.5", "mockery"))
	require.True(t, os.IsNotExist(err), "nothing is keyed by name alone any more")
}

// unlockable refuses every lock: the store cannot be claimed.
type unlockable struct{}

func (unlockable) Lock(string) (func(), error) {
	return nil, errors.New("the store is on a filesystem that cannot lock")
}

// A store that cannot be locked is not built into: two builds racing with no
// lock is the silent last-writer-wins the lock exists to remove, so the
// provision fails naming the store rather than proceeding without it.
func TestAStoreThatCannotBeLockedIsNotBuiltInto(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t) // no expectation: nothing may build

	_, err := toolingcontroller.New(fsadapter.New(), runner, unlockable{}).
		ProvisionBinaries(context.Background(), t.TempDir(), t.TempDir(), []toolingcontroller.Binary{mockery})
	require.ErrorContains(t, err, "locking the store for mockery")
	require.ErrorContains(t, err, "cannot lock")
}

// Two workspaces provisioning the same tool at once build it once. The
// loser of the race finds the winner's entry under the lock and reuses it,
// where before both built and the last symlink silently won.
func TestConcurrentProvisionsOfOneBinaryBuildItOnce(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	runner := execadaptermock.NewMockRunner(t)
	goInstallFake(t, runner, "built-once") // .Once(): a second go install fails the test

	c := toolingcontroller.New(fsadapter.New(), runner, lockadapter.New())

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		reports []toolingcontroller.BinaryReport
	)

	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			report, err := c.ProvisionBinaries(context.Background(), t.TempDir(), store,
				[]toolingcontroller.Binary{mockery})
			require.NoError(t, err)

			mu.Lock()
			reports = append(reports, report)
			mu.Unlock()
		}()
	}

	wg.Wait()

	installed := 0
	for _, r := range reports {
		installed += len(r.Installed)
	}

	assert.Equal(t, 1, installed, "one build; every other provision reused it")
	assert.Len(t, reports, 4)
}
