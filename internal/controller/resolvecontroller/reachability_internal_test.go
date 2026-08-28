package resolvecontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeForge puts an executable named "forge" first on PATH, so the real
// function resolves through PATH exactly as it does in production. A seam in
// production code that exists only for a test would prove less.
func fakeForge(t *testing.T, script string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "forge"),
		[]byte("#!/bin/sh\n"+script+"\n"), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return dir
}

func TestRunVulncheckEngineNamesEveryModule(t *testing.T) {
	dir := fakeForge(t, `printf '%s\n' "$*" > "$0.args"; echo '{"depth":"imports"}'`)

	out, err := RunVulncheckEngine(context.Background(),
		[]string{"/a", "/b"}, []byte(`{"findings":[]}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"depth":"imports"}`, string(out))

	args, err := os.ReadFile(filepath.Join(dir, "forge.args"))
	require.NoError(t, err)

	// Every module, and the engine named as a module rather than a binary on
	// PATH: that is what makes the version governed rather than whatever is
	// installed.
	require.Contains(t, string(args), "--dir /a --dir /b")
	require.Contains(t, string(args), "run github.com/alexandremahdhaoui/forge-go-vulncheck")
}

func TestRunVulncheckEngineReadsStdin(t *testing.T) {
	fakeForge(t, `cat`)

	payload := `{"findings":[{"id":"X-1","imports":["p/q"]}]}`

	out, err := RunVulncheckEngine(context.Background(), []string{"/a"}, []byte(payload))
	require.NoError(t, err)
	require.JSONEq(t, payload, string(out))
}

func TestRunVulncheckEngineFailureIsAnError(t *testing.T) {
	fakeForge(t, `exit 3`)

	_, err := RunVulncheckEngine(context.Background(), []string{"/a"}, nil)
	require.ErrorContains(t, err, "running the reachability engine")
}

func TestRunVulncheckEngineRefusesARunawayEngine(t *testing.T) {
	// Well past the cap, written in one go so the writer sees it as one
	// oversized chunk the way a real report would arrive.
	fakeForge(t, `head -c 5242880 /dev/zero | tr '\0' 'x'`)

	_, err := RunVulncheckEngine(context.Background(), []string{"/a"}, nil)
	require.ErrorContains(t, err, "running the reachability engine")
}

func TestLimitedWriterStopsAtTheCap(t *testing.T) {
	var buf bytes.Buffer

	w := &limitedWriter{w: &buf, left: 4}

	n, err := w.Write([]byte("ab"))
	require.NoError(t, err)
	require.Equal(t, 2, n)

	// The remainder is what is left, not what was asked for.
	_, err = w.Write([]byte("cde"))
	require.ErrorContains(t, err, "wrote more than")

	// The rejected chunk is not half written. A truncated JSON report parses
	// as garbage and would be reported as an unreadable answer, hiding the
	// real cause.
	require.Equal(t, "ab", buf.String())
}

func TestReachabilityLinesFromARealisticReport(t *testing.T) {
	r := reachability{run: func(_ context.Context, dirs []string, stdin []byte) ([]byte, error) {
		require.Equal(t, []string{"/m"}, dirs)

		var in struct {
			Findings []finding `json:"findings"`
		}

		require.NoError(t, json.Unmarshal(stdin, &in))
		require.Len(t, in.Findings, 2)

		return []byte(`{"depth":"imports","answers":[
			{"id":"A-1","verdict":"not-reached","reason":"no package under x/ is imported"},
			{"id":"A-2","verdict":"reached","via":["m/b","m/a"]}]}`), nil
	}}

	got := r.lines(context.Background(), []string{"/m"},
		[]string{"A-1", "A-2"}, []string{"x/y"})

	require.Contains(t, got["A-1"], "your code does not reach it: no package under x/ is imported")
	// The depth travels with the answer, because import depth makes
	// not-reached strong and reached weak, and a reader has to know which.
	require.Contains(t, got["A-1"], "imports")
	// Sorted, so the sentence does not change between runs.
	require.Equal(t, "your code reaches it, through m/a, m/b", got["A-2"])
}

func TestReachabilityLinesSayNothingRatherThanGuess(t *testing.T) {
	called := false
	ok := func(_ context.Context, _ []string, _ []byte) ([]byte, error) {
		called = true

		return []byte(`{"answers":[{"id":"A-1","verdict":"not-reached"}]}`), nil
	}

	for name, tc := range map[string]struct {
		r                  reachability
		dirs, ids, imports []string
	}{
		"no engine":  {reachability{}, []string{"/m"}, []string{"A-1"}, []string{"x"}},
		"no module":  {reachability{run: ok}, nil, []string{"A-1"}, []string{"x"}},
		"no finding": {reachability{run: ok}, []string{"/m"}, nil, []string{"x"}},
		"no scope":   {reachability{run: ok}, []string{"/m"}, []string{"A-1"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			called = false
			require.Nil(t, tc.r.lines(context.Background(), tc.dirs, tc.ids, tc.imports))
			require.False(t, called, "asked the engine a question it cannot answer")
		})
	}
}

func TestReachabilityLinesIgnoreAnUnreadableAnswer(t *testing.T) {
	for name, out := range map[string]string{
		"not json":        `{{{`,
		"unscoped":        `{"depth":"imports","answers":[{"id":"A-1","verdict":"unscoped"}]}`,
		"unknown verdict": `{"depth":"imports","answers":[{"id":"A-1","verdict":"maybe"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := reachability{run: func(_ context.Context, _ []string, _ []byte) ([]byte, error) {
				return []byte(out), nil
			}}

			got := r.lines(context.Background(), []string{"/m"}, []string{"A-1"}, []string{"x"})
			require.Empty(t, got, "spoke for a finding the engine did not answer")
		})
	}
}

func TestReachabilityLinesSurviveAFailingEngine(t *testing.T) {
	r := reachability{run: func(_ context.Context, _ []string, _ []byte) ([]byte, error) {
		return nil, errors.New("engine not provisioned")
	}}

	require.Nil(t, r.lines(context.Background(), []string{"/m"}, []string{"A-1"}, []string{"x"}))
}
