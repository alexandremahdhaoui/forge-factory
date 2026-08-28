package engineadapter

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	buildOnce  sync.Once
	enginePath string
	buildErr   error
)

// fakeEngine builds the test engine once and hands back a caller pointed at
// it. Building it is the point: the failures this covers all live in the
// transport, and a mocked session would only prove that the parser reads what
// the test wrote.
func fakeEngine(t *testing.T, stderr *bytes.Buffer) *MCPCaller {
	t.Helper()

	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeengine")
		if err != nil {
			buildErr = err

			return
		}

		enginePath = filepath.Join(dir, "fakeengine")

		cmd := exec.Command("go", "build", "-o", enginePath, "./testdata/fakeengine")

		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("building the test engine: %s", out)
		}
	})
	require.NoError(t, buildErr)

	var w = stderr
	if w == nil {
		w = &bytes.Buffer{}
	}

	c := NewMCPCaller("", "v0.0.0-test", w)
	c.resolver.LookPath = func(string) (string, error) { return enginePath, nil }

	return c
}

func TestCallRoundTripsOverTheWire(t *testing.T) {
	var out struct {
		Word string `json:"word"`
	}

	require.NoError(t, fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "echo", map[string]any{"word": "hello"}, &out))
	require.Equal(t, "hello", out.Word)
}

func TestCallAcceptsAStructAsArguments(t *testing.T) {
	in := struct {
		Word string `json:"word"`
	}{Word: "typed"}

	var out map[string]any

	require.NoError(t, fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "echo", in, &out))
	require.Equal(t, "typed", out["word"])
}

func TestCallDiscardsTheResultWhenNobodyAsked(t *testing.T) {
	// A tool called for its effect answers nothing structured, and asking for
	// nothing must not turn that into a failure.
	require.NoError(t, fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "say-nothing", nil, nil))
}

func TestCallSurfacesTheEnginesOwnReason(t *testing.T) {
	err := fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "refuse", nil, nil)

	// The engine's sentence, not the transport's. An operator reading this
	// needs to know what the engine objected to.
	require.ErrorContains(t, err, "the engine refused")
	require.ErrorContains(t, err, "calling refuse on forge://fakeengine")
}

func TestCallSaysSoWhenTheEngineGivesNoReason(t *testing.T) {
	err := fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "refuse-silently", nil, nil)

	// A failure rendered as an empty string reads like a bug in us.
	require.ErrorContains(t, err, "engine reported an error with no message")
}

func TestCallRefusesAnEmptyAnswerWhenOneWasAsked(t *testing.T) {
	var out map[string]any

	err := fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "say-nothing", nil, &out)
	require.ErrorContains(t, err, "engine returned no structured content")
}

func TestCallNamesADecodeFailure(t *testing.T) {
	var out struct {
		Word string `json:"word"`
	}

	err := fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "wrong-shape", nil, &out)
	require.ErrorContains(t, err, "decoding result of wrong-shape on forge://fakeengine")
}

func TestCallReportsATransportFailure(t *testing.T) {
	err := fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "explode", nil, nil)
	require.ErrorContains(t, err, "calling explode on forge://fakeengine")
}

func TestCallReportsAnUnknownTool(t *testing.T) {
	err := fakeEngine(t, nil).Call(context.Background(),
		"forge://fakeengine", "no-such-tool", nil, nil)
	require.ErrorContains(t, err, "no-such-tool")
}

func TestCallRefusesAnUnresolvableURI(t *testing.T) {
	err := fakeEngine(t, nil).Call(context.Background(), "https://nope", "echo", nil, nil)
	require.ErrorIs(t, err, ErrScheme)
}

func TestCallCannotConnectToSomethingThatIsNotAnEngine(t *testing.T) {
	c := NewMCPCaller("", "v0.0.0-test", nil)
	c.resolver.LookPath = func(string) (string, error) { return "/bin/true", nil }

	err := c.Call(context.Background(), "forge://nothing", "echo", nil, nil)
	require.ErrorContains(t, err, "connecting to engine forge://nothing")
}

func TestTheEnginesStderrReachesTheWriterItWasGiven(t *testing.T) {
	// Engine logs go to stderr by contract, because stdout carries the
	// JSON-RPC stream. Dropping them leaves a broken engine with nothing to
	// debug from, which is exactly when somebody needs them.
	dir := t.TempDir()
	script := filepath.Join(dir, "noisy")
	require.NoError(t, os.WriteFile(script,
		[]byte("#!/bin/sh\necho 'engine could not start' >&2\nexit 1\n"), 0o700))

	var seen bytes.Buffer

	c := NewMCPCaller("", "v0.0.0-test", &seen)
	c.resolver.LookPath = func(string) (string, error) { return script, nil }

	err := c.Call(context.Background(), "forge://noisy", "echo", nil, nil)
	require.Error(t, err)
	require.Contains(t, seen.String(), "engine could not start")
}

func TestNewMCPCallerNeverWritesToANilWriter(t *testing.T) {
	require.NotNil(t, NewMCPCaller("", "v", nil).stderr)
}

func TestToArgumentsCarriesEveryShapeACallerHas(t *testing.T) {
	got, err := toArguments(nil)
	require.NoError(t, err)
	require.Equal(t, map[string]any{}, got)

	// A map passes through untouched rather than round-tripping through
	// JSON, which would turn every number into a float64.
	m := map[string]any{"n": 1}
	got, err = toArguments(m)
	require.NoError(t, err)
	require.Equal(t, 1, got["n"])

	// A value that is valid JSON but not an object has no arguments shape.
	_, err = toArguments([]int{1})
	require.Error(t, err)

	_, err = toArguments(make(chan int))
	require.Error(t, err)
}
