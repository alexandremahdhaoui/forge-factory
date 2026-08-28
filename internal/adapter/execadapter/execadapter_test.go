package execadapter_test

import (
	"context"
	"os"
	"path/filepath"
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

func TestLookPathAnswersWhatPATHHolds(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a-tool"), []byte("#!/bin/sh\n"), 0o700))
	t.Setenv("PATH", dir)

	got, ok := execadapter.New().LookPath("a-tool")
	require.True(t, ok)
	require.Equal(t, filepath.Join(dir, "a-tool"), got)

	// Absent is false and never an error: the caller's whole question is
	// whether to exec this or fall back, and a fallback is not a failure.
	_, ok = execadapter.New().LookPath("no-such-tool")
	require.False(t, ok)
}

func TestLookPathRefusesAFileItCannotRun(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a-tool"), []byte("x"), 0o600))
	t.Setenv("PATH", dir)

	// Present but not executable. Answering yes here would pick a command
	// that fails at exec time, well past the point the fallback was possible.
	_, ok := execadapter.New().LookPath("a-tool")
	require.False(t, ok)
}
