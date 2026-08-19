package engineadapter_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/stretchr/testify/require"
)

func notOnPath(string) (string, error) { return "", errors.New("not found") }

func TestAnInstalledBinaryWins(t *testing.T) {
	r := engineadapter.NewResolver("")
	r.LookPath = func(name string) (string, error) {
		require.Equal(t, "ci-compute-local", name)

		return "/usr/local/bin/ci-compute-local", nil
	}

	cmd, err := r.Resolve("go://github.com/x/forge-factory/cmd/ci-compute-local@v0.1.0")
	require.NoError(t, err)
	require.Equal(t, "/usr/local/bin/ci-compute-local", cmd.Path)
	require.Empty(t, cmd.Args)
}

func TestTheSourceTreeIsUsedWhenNothingIsInstalled(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cmd", "ci-state-git"), 0o750))

	r := engineadapter.NewResolver(dir)
	r.LookPath = notOnPath

	cmd, err := r.Resolve("go://github.com/x/forge-factory/cmd/ci-state-git@v0.1.0")
	require.NoError(t, err)
	require.Equal(t, "go", cmd.Path)
	require.Equal(t, []string{"run", "./cmd/ci-state-git"}, cmd.Args)
}

func TestAModuleFallsBackToGoRun(t *testing.T) {
	r := engineadapter.NewResolver("")
	r.LookPath = notOnPath

	cmd, err := r.Resolve("go://github.com/x/forge-factory/cmd/ci-gate-manual@v0.2.0")
	require.NoError(t, err)
	require.Equal(t, "go", cmd.Path)
	require.Equal(t, []string{"run", "github.com/x/forge-factory/cmd/ci-gate-manual@v0.2.0"}, cmd.Args)
}

func TestAShortNameResolvesToOurOwnModule(t *testing.T) {
	r := engineadapter.NewResolver("")
	r.LookPath = notOnPath

	cmd, err := r.Resolve("go://ci-promotion-all")
	require.NoError(t, err)
	require.Equal(t,
		[]string{"run", "github.com/alexandremahdhaoui/forge-factory/cmd/ci-promotion-all@latest"},
		cmd.Args)
}

func TestAMissingVersionBecomesLatest(t *testing.T) {
	r := engineadapter.NewResolver("")
	r.LookPath = notOnPath

	cmd, err := r.Resolve("go://github.com/x/y/cmd/z")
	require.NoError(t, err)
	require.Equal(t, []string{"run", "github.com/x/y/cmd/z@latest"}, cmd.Args)
}

func TestAliasMustBeResolvedFirst(t *testing.T) {
	_, err := engineadapter.NewResolver("").Resolve("alias://my-engine")
	require.ErrorIs(t, err, engineadapter.ErrAlias)
}

func TestOtherSchemesAreRefused(t *testing.T) {
	for _, uri := range []string{"https://example.com/x", "ci-compute-local", "go://", ""} {
		_, err := engineadapter.NewResolver("").Resolve(uri)
		require.ErrorIs(t, err, engineadapter.ErrScheme, uri)
	}
}

func TestASourceDirWithoutTheCommandFallsThrough(t *testing.T) {
	r := engineadapter.NewResolver(t.TempDir())
	r.LookPath = notOnPath

	cmd, err := r.Resolve("go://github.com/x/y/cmd/missing@v1")
	require.NoError(t, err)
	require.Equal(t, []string{"run", "github.com/x/y/cmd/missing@v1"}, cmd.Args)
}

func TestANilLookPathIsSkipped(t *testing.T) {
	r := engineadapter.NewResolver("")
	r.LookPath = nil

	cmd, err := r.Resolve("go://github.com/x/y/cmd/z@v1")
	require.NoError(t, err)
	require.Equal(t, "go", cmd.Path)
}
