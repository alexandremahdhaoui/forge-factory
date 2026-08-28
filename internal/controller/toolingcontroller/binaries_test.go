package toolingcontroller_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/toolingcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
)

// goInstallFake plays go install: it writes the built binary into the
// GOBIN the controller staged, which is exactly the contract the real
// tool honours.
func goInstallFake(t *testing.T, runner *execadaptermock.MockRunner, content string) {
	t.Helper()

	runner.EXPECT().
		RunEnv(mock.Anything, "", mock.Anything, "go", "install",
			"github.com/vektra/mockery/v3@v3.5.5").
		RunAndReturn(func(_ context.Context, _ string, env map[string]string, _ string, _ ...string) (execadapter.Result, error) {
			require.Equal(t, "off", env["GOWORK"])
			require.NoError(t, os.WriteFile(filepath.Join(env["GOBIN"], "v3"), []byte(content), 0o755))

			return execadapter.Result{}, nil
		}).Once()
}

func TestProvisionBinariesBuildsIntoTheStoreAndLinks(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	root := t.TempDir()
	runner := execadaptermock.NewMockRunner(t)
	goInstallFake(t, runner, "the-built-binary")

	c := toolingcontroller.New(fsadapter.New(), runner)

	report, err := c.ProvisionBinaries(context.Background(), root, store,
		[]toolingcontroller.Binary{{
			Name: "mockery", Module: "github.com/vektra/mockery/v3", Version: "v3.5.5",
		}})
	require.NoError(t, err)
	assert.Equal(t, []string{"mockery"}, report.Installed)

	// The workspace link resolves through the store to the built bytes.
	built, err := os.ReadFile(filepath.Join(root, ".forge", "bin", "mockery"))
	require.NoError(t, err)
	assert.Equal(t, "the-built-binary", string(built))

	info, err := os.Stat(filepath.Join(root, ".forge", "bin", "mockery"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "a provisioned tool must execute")

	// A second provision reuses the (module, version) build: no runner call.
	again, err := toolingcontroller.New(fsadapter.New(), execadaptermock.NewMockRunner(t)).
		ProvisionBinaries(context.Background(), root, store,
			[]toolingcontroller.Binary{{
				Name: "mockery", Module: "github.com/vektra/mockery/v3", Version: "v3.5.5",
			}})
	require.NoError(t, err)
	assert.Equal(t, []string{"mockery"}, again.Reused)
}

// Latest is never a fallback, here like everywhere.
func TestProvisionBinariesRefusesAFloatingVersion(t *testing.T) {
	t.Parallel()

	c := toolingcontroller.New(fsadapter.New(), execadaptermock.NewMockRunner(t))

	for _, version := range []string{"", "latest"} {
		_, err := c.ProvisionBinaries(context.Background(), t.TempDir(), t.TempDir(),
			[]toolingcontroller.Binary{{Name: "x", Module: "m/x", Version: version}})
		require.ErrorContains(t, err, "latest is never a fallback", version)
	}
}

func TestProvisionBinariesSurfacesABuildFailure(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().RunEnv(mock.Anything, "", mock.Anything, "go", "install", "m/x@v1.0.0").
		Return(execadapter.Result{ExitCode: 1, Stderr: "no required module"}, nil).Once()

	c := toolingcontroller.New(fsadapter.New(), runner)

	_, err := c.ProvisionBinaries(context.Background(), t.TempDir(), t.TempDir(),
		[]toolingcontroller.Binary{{Name: "x", Module: "m/x", Version: "v1.0.0"}})
	require.ErrorContains(t, err, "exited 1")
	require.ErrorContains(t, err, "no required module")
}

func TestProvisionBinariesWithNothingDeclaredDoesNothing(t *testing.T) {
	t.Parallel()

	report, err := toolingcontroller.New(fsadapter.New(), execadaptermock.NewMockRunner(t)).
		ProvisionBinaries(context.Background(), t.TempDir(), t.TempDir(), nil)
	require.NoError(t, err)
	assert.Empty(t, report.Installed)
	assert.Empty(t, report.Reused)
}

// Two workspaces on one machine, or two CI jobs sharing a cached store,
// syncing at once with different pins for the same tool name. The staging
// GOBIN was keyed on that name alone, so the two builds shared a directory:
// the loser read the winner's binary and symlinked it under its own version,
// permanently, because the tools path is reused forever after.
func TestTwoVersionsOfOneToolDoNotShareAStagingDir(t *testing.T) {
	t.Parallel()

	store := t.TempDir()

	// The fake blocks until both builds have staged, which is the interleave
	// the shared directory made possible. With a shared dir this deadlocks or
	// crosses; with a keyed one both proceed.
	staged := make(chan string, 2)
	release := make(chan struct{})

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().
		RunEnv(mock.Anything, "", mock.Anything, "go", "install", mock.Anything).
		RunAndReturn(func(_ context.Context, _ string, env map[string]string, _ string, args ...string) (execadapter.Result, error) {
			version := args[len(args)-1]
			staged <- env["GOBIN"]
			<-release
			require.NoError(t, os.WriteFile(filepath.Join(env["GOBIN"], "toolx"),
				[]byte("built-"+version), 0o755))

			return execadapter.Result{}, nil
		}).Twice()

	c := toolingcontroller.New(fsadapter.New(), runner)

	type result struct {
		root string
		err  error
	}

	done := make(chan result, 2)

	for _, v := range []string{"v1.0.0", "v2.0.0"} {
		go func() {
			root := t.TempDir()
			_, err := c.ProvisionBinaries(context.Background(), root, store,
				[]toolingcontroller.Binary{{
					Name: "toolx", Module: "example.com/toolx", Version: v,
				}})
			done <- result{root: root, err: err}
		}()
	}

	first, second := <-staged, <-staged
	require.NotEqual(t, first, second, "both builds staged into one directory")

	close(release)

	for range 2 {
		r := <-done
		require.NoError(t, r.err)

		got, err := os.ReadFile(filepath.Join(r.root, ".forge", "bin", "toolx"))
		require.NoError(t, err)

		// Each workspace gets the version it pinned. The store keeps a
		// resolved path forever, so a crossed link here is not a slow run,
		// it is the wrong binary for the life of the machine.
		assert.Contains(t, []string{"built-example.com/toolx@v1.0.0", "built-example.com/toolx@v2.0.0"},
			string(got))
	}
}
