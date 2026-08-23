package execadapter_test

import (
	"context"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/stretchr/testify/require"
)

func TestStdoutAndStderrAreCaptured(t *testing.T) {
	res, err := execadapter.New().Run(context.Background(), "", "sh", "-c", "echo out; echo err >&2")
	require.NoError(t, err)
	require.Equal(t, "out\n", res.Stdout)
	require.Equal(t, "err\n", res.Stderr)
	require.Equal(t, 0, res.ExitCode)
}

func TestANonZeroExitIsNotAnError(t *testing.T) {
	res, err := execadapter.New().Run(context.Background(), "", "sh", "-c", "exit 3")
	require.NoError(t, err)
	require.Equal(t, 3, res.ExitCode)
}

func TestTheWorkingDirectoryIsHonoured(t *testing.T) {
	dir := t.TempDir()

	res, err := execadapter.New().Run(context.Background(), dir, "pwd")
	require.NoError(t, err)
	require.Contains(t, res.Stdout, dir)
}

func TestAMissingBinaryIsAnError(t *testing.T) {
	_, err := execadapter.New().Run(context.Background(), "", "definitely-not-a-real-binary-xyz")
	require.Error(t, err)
	require.Contains(t, err.Error(), "running definitely-not-a-real-binary-xyz")
}

func TestRunAttachedAnswersTheExitCode(t *testing.T) {
	t.Parallel()

	code, err := execadapter.New().RunAttached(context.Background(), "", nil, "sh", "-c", "exit 4")
	require.NoError(t, err)
	require.Equal(t, 4, code)

	code, err = execadapter.New().RunAttached(context.Background(), "",
		map[string]string{"X": "y"}, "sh", "-c", `test "$X" = y`)
	require.NoError(t, err)
	require.Zero(t, code)

	_, err = execadapter.New().RunAttached(context.Background(), "", nil, "/does/not/exist")
	require.Error(t, err)
}
