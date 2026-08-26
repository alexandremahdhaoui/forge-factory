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
