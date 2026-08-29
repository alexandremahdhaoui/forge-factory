package synccontroller_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/toolingcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/engineadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/fsadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/repoadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const factory = `version: "1"
name: golden
repos:
  - name: golden-go
    url: u
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
dependencies:
  go:
    sigs.k8s.io/yaml: v1.6.0
`

func parse(t *testing.T, raw string) config.Factory {
	t.Helper()

	f, err := config.Parse([]byte(raw))
	require.NoError(t, err)

	return f
}

// passthrough resolves like the pre-register world: legacy versions pass
// through verbatim.
type passthrough struct{}

func (passthrough) Resolve(_ context.Context, _ config.Factory, _, _ string, deps map[string]config.DependencySpec) (map[string]string, []string, error) {
	out := make(map[string]string, len(deps))
	for name, d := range deps {
		out[name] = d.Version
	}

	return out, nil, nil
}

// ResolveTool answers a fixed version, so tests can see a track resolve
// without a register checkout.
func (passthrough) ResolveTool(context.Context, config.Factory, string, string) (string, []string, error) {
	return "v9.9.9", []string{"tool note"}, nil
}

// The pre-register world had no internal tracks, so nothing is pinned and
// the tidy that follows keeps whatever it had.
func (passthrough) ResolveMembers(
	context.Context, config.Factory, string, string,
) (map[string]string, error) {
	return map[string]string{}, nil
}

type harness struct {
	caller *engineadaptermock.MockCaller
	fs     *fsadaptermock.MockFS
	repos  *repoadaptermock.MockReader
	runner *execadaptermock.MockRunner
	c      *synccontroller.Controller
	wrote  map[string]string
	// goMods is what each member's committed manifest says. Sync reads it
	// to learn which internal modules a member already requires.
	goMods map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		caller: engineadaptermock.NewMockCaller(t),
		fs:     fsadaptermock.NewMockFS(t),
		repos:  repoadaptermock.NewMockReader(t),
		runner: execadaptermock.NewMockRunner(t),
		wrote:  map[string]string{},
		goMods: map[string]string{},
	}

	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner, passthrough{})

	// Sync reads each member's committed go.mod to learn which internal
	// modules it already requires. Absent is the common case in these
	// tests and means "requires nothing", which is what a member with no
	// manifest yet actually is.
	h.fs.EXPECT().ReadFile(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/go.mod")
	})).RunAndReturn(func(path string) ([]byte, error) {
		if body, ok := h.goMods[path]; ok {
			return []byte(body), nil
		}

		return nil, errors.New("no manifest yet")
	}).Maybe()

	return h
}

// envrcExists answers the pre-sync existence probe for every member's
// .envrc; most tests want them present, already carrying the managed
// tooling PATH line, so no write is recorded.
func (h *harness) envrcExists(exists bool) {
	isEnvrc := func(path string) bool {
		return strings.HasSuffix(path, "/.envrc")
	}

	h.fs.EXPECT().Exists(mock.MatchedBy(isEnvrc)).Return(exists, nil).Maybe()

	if exists {
		h.fs.EXPECT().ReadFile(mock.MatchedBy(isEnvrc)).
			Return([]byte("export PATH=\"/w/.forge/bin:$PATH\" # forge-factory: workspace tooling\n"), nil).
			Maybe()
	}
}

func (h *harness) recordWrites() {
	h.fs.EXPECT().WriteFile(mock.Anything, mock.Anything).
		RunAndReturn(func(path string, data []byte) error {
			h.wrote[path] = string(data)

			return nil
		}).Maybe()
}

func (h *harness) answers(uri, tool string, payload any) {
	h.caller.EXPECT().Call(mock.Anything, uri, tool, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			raw, err := json.Marshal(payload)
			if err != nil {
				return err
			}

			return json.Unmarshal(raw, out)
		}).Once()
}

func TestSyncWritesWhatTheEngineAsksForAndIgnoresIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "module example.com/g\n", "gitignore": "golden-go"},
		{"path": "/w/go.work", "content": "use ./golden-go\n"},
	}})
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(false, nil).Once()
	h.recordWrites()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"/w/go.work", "/w/golden-go/go.mod"}, report.Written)
	assert.Equal(t, []string{"/w/golden-go/.gitignore"}, report.Ignored)
	assert.Equal(t, "module example.com/g\n", h.wrote["/w/golden-go/go.mod"])
	assert.Contains(t, h.wrote["/w/golden-go/.gitignore"], "/go.mod")
	assert.Contains(t, h.wrote["/w/golden-go/.gitignore"], "forge-factory materialises these")
}

func TestSyncSendsTheDependenciesAndIdentityToTheEngine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})

	var seen map[string]any

	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "render", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &seen))

			return json.Unmarshal([]byte(`{"files":[]}`), out)
		}).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	assert.Equal(t, "/w", seen["root"])
	assert.Equal(t, map[string]any{"sigs.k8s.io/yaml": "v1.6.0"}, seen["dependencies"])

	repos, ok := seen["repos"].([]any)
	require.True(t, ok)
	require.Len(t, repos, 1)

	repo, ok := repos[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/w/golden-go", repo["path"])
	assert.Equal(t, map[string]any{"module": "example.com/g"}, repo["identity"])
}

func TestSyncRefusesAnEngineThatSpeaksAnotherLanguage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "rust"})

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, synccontroller.ErrLanguage)
	assert.Contains(t, err.Error(), `registered as "go" but speaks "rust"`)
}

func TestSyncAddsToAGitignoreWithoutDisturbingIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/golden-go/.gitignore").Return([]byte(".envrc\ntmp/"), nil).Once()
	h.recordWrites()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	got := h.wrote["/w/golden-go/.gitignore"]
	assert.Contains(t, got, ".envrc")
	assert.Contains(t, got, "tmp/")
	assert.Contains(t, got, "/go.mod")
}

func TestSyncLeavesAGitignoreAloneWhenItAlreadyNamesEverything(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/golden-go/.gitignore").Return([]byte("/go.mod\n"), nil).Once()
	h.fs.EXPECT().WriteFile("/w/golden-go/go.mod", mock.Anything).Return(nil).Once()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Empty(t, report.Ignored)
}

func TestSyncFailsWhenARepoCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(nil, assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
}

func TestSyncFailsWhenAnEngineFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "render", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "rendering go")
}

func TestSyncFailsWhenTheLanguageProbeFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "language", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "what language it speaks")
}

func TestSyncFailsWhenAFileCannotBeWritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/go.work", "content": "x"},
	}})
	h.fs.EXPECT().WriteFile("/w/go.work", mock.Anything).Return(assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
}

func TestSyncFailsWhenAGitignoreCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.fs.EXPECT().WriteFile("/w/golden-go/go.mod", mock.Anything).Return(nil).Once()
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(false, assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
}

func TestSyncRefusesALanguageWithNoEngine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()

	f := parse(t, factory)
	f.Engines = nil

	_, err := h.c.Sync(t.Context(), f, "/w", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no engine is registered")
}

func TestTheLanguageProbeNeverSendsNull(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()

	var raw []byte

	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "language", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			var err error

			raw, err = json.Marshal(in)
			require.NoError(t, err)

			return json.Unmarshal([]byte(`{"language":"go"}`), out)
		}).Once()
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []any{}})

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	assert.NotContains(t, string(raw), "null",
		"a nil slice or map travels as null and the engine's own schema refuses it")
}

func TestSyncRunsWhatAnEngineAsksForAfterTheFilesLand(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files": []map[string]any{{"path": "/w/golden-go/go.mod", "content": "x"}},
		"settle": []map[string]any{{
			"dir": "/w/golden-go", "command": "go", "args": []string{"mod", "tidy"},
			"env": map[string]string{"GOWORK": "off"}, "optional": true,
		}},
	})
	h.recordWrites()
	h.runner.EXPECT().RunEnv(mock.Anything, "/w/golden-go", map[string]string{"GOWORK": "off"},
		"go", "mod", "tidy").Return(execadapter.Result{}, nil).Once()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"go mod tidy in /w/golden-go"}, report.Settled)
	assert.Empty(t, report.Unsettled)
}

func TestAnOptionalCommandThatFailsIsReportedAndTheSyncStillPasses(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files": []any{},
		"settle": []map[string]any{
			{"dir": "/w/golden-go", "command": "go", "args": []string{"mod", "tidy"}, "optional": true},
		},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, mock.Anything, mock.Anything, "go", "mod", "tidy").
		Return(execadapter.Result{}, assert.AnError).Once()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err, "a tidy needs the network and a sync must work offline")
	require.Len(t, report.Unsettled, 1)
	assert.Contains(t, report.Unsettled[0], "go mod tidy in /w/golden-go")
}

func TestACommandThatIsNotOptionalStopsTheSync(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":  []any{},
		"settle": []map[string]any{{"dir": "/w", "command": "false"}},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, "/w", mock.Anything, "false").
		Return(execadapter.Result{}, assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "running false")
}

func TestACommandThatExitsNonZeroCountsAsAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":  []any{},
		"settle": []map[string]any{{"dir": "/w", "command": "go", "args": []string{"mod", "tidy"}}},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, "/w", mock.Anything, "go", "mod", "tidy").
		Return(execadapter.Result{ExitCode: 1, Stderr: "unknown directive: #"}, nil).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, synccontroller.ErrCommand,
		"a non zero exit comes back with no error, so reading only the error passes every failure")
	assert.Contains(t, err.Error(), "unknown directive")
}

func TestALongFailureIsTrimmedToItsReason(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":  []any{},
		"settle": []map[string]any{{"dir": "/w", "command": "go"}},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, "/w", mock.Anything, "go").
		Return(execadapter.Result{
			ExitCode: 2,
			Stderr:   strings.Repeat("x", 600) + "the reason",
		}, nil).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the reason")
	assert.Less(t, len(err.Error()), 600)
}

func TestAFileCanNameMoreThanItselfToIgnore(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{{
		"path": "/w/golden-go/go.mod", "content": "x",
		"gitignore": "golden-go", "alsoIgnore": []string{"go.sum"},
	}}})
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(false, nil).Once()
	h.recordWrites()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	got := h.wrote["/w/golden-go/.gitignore"]
	assert.Contains(t, got, "/go.mod")
	assert.Contains(t, got, "/go.sum")
}

const twoMemberFactory = `version: "1"
name: golden
repos:
  - name: golden-go
    url: u
    languages: [go]
  - name: other-go
    url: v
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
`

func TestSyncOnlyRendersTheOneMemberAndTheRoot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files": []map[string]any{
			{"path": "/w/golden-go/go.mod", "content": "module example.com/g\n"},
			{"path": "/w/other-go/go.mod", "content": "module example.com/o\n"},
			{"path": "/w/go.work", "content": "use ./golden-go\n"},
		},
		"settle": []map[string]any{
			{"dir": "/w/golden-go", "command": "true"},
			{"dir": "/w/other-go", "command": "false"},
			{"dir": "/w", "command": "true"},
		},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, "/w/golden-go", mock.Anything, "true").
		Return(execadapter.Result{}, nil).Once()
	h.runner.EXPECT().RunEnv(mock.Anything, "/w", mock.Anything, "true").
		Return(execadapter.Result{}, nil).Once()
	h.recordWrites()

	report, err := h.c.Sync(t.Context(), parse(t, twoMemberFactory), "/w", "golden-go")
	require.NoError(t, err)

	assert.Equal(t, []string{"/w/go.work", "/w/golden-go/go.mod"}, report.Written,
		"the one member and the root land; the absent member never does")
	assert.NotContains(t, h.wrote, "/w/other-go/go.mod")
}

// Toolchain binaries resolve during sync: a literal pin as written, a
// track through the register like any dependency, with the notes surfaced.
func TestSyncResolvesTheToolchainBinaries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	f := config.Factory{
		Repos: []config.Repo{{Name: "member-a"}},
		Toolchain: &config.Toolchain{Binaries: []config.ToolchainBinary{
			{Name: "pinned-tool", Module: "example.com/x/cmd/pinned-tool", Version: "v1.2.3"},
			{Name: "tracked-tool", Module: "example.com/y/cmd/tracked-tool", Track: "go:example.com/y"},
		}},
	}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	require.Equal(t, []toolingcontroller.Binary{
		{Name: "pinned-tool", Module: "example.com/x/cmd/pinned-tool", Version: "v1.2.3"},
		{Name: "tracked-tool", Module: "example.com/y/cmd/tracked-tool", Version: "v9.9.9"},
	}, report.Toolchain)
	require.Contains(t, report.Notes, "tool note")
}

// TestSyncCreatesAMissingEnvrc: forge sources a .envrc in every repo it
// builds, so a fresh workspace used to need a hand-run touch loop. Sync
// creates the missing file carrying exactly the managed tooling PATH line;
// everything else in an existing file is the machine's own.
func TestSyncCreatesAMissingEnvrc(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(false)
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}, {Name: "member-b"}}}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	require.Equal(t, "export PATH=\"/w/.forge/bin:$PATH\" # forge-factory: workspace tooling\n",
		h.wrote["/w/member-a/.envrc"])
	require.Contains(t, report.Written, "/w/member-a/.envrc")
	require.Contains(t, report.Written, "/w/member-b/.envrc")
}

// TestSyncAppendsTheToolingLineToAnExistingEnvrc: an existing .envrc keeps
// every line the machine put there; sync appends only the managed tooling
// PATH entry, once.
func TestSyncAppendsTheToolingLineToAnExistingEnvrc(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fs.EXPECT().Exists(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/.envrc")
	})).Return(true, nil).Maybe()
	h.fs.EXPECT().ReadFile("/w/member-a/.envrc").
		Return([]byte("export MY_OWN=1"), nil).Once()
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}}}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	require.Equal(t,
		"export MY_OWN=1\nexport PATH=\"/w/.forge/bin:$PATH\" # forge-factory: workspace tooling\n",
		h.wrote["/w/member-a/.envrc"])
	require.Contains(t, report.Written, "/w/member-a/.envrc")
}

// failingResolver stands in for a register that refuses. Every one of these
// paths returned the resolver's error unwrapped, so the operator saw a
// package name with no clue which of the four lists it came from.
type failingResolver struct {
	err error
	// after says how many calls succeed first, so the devDependencies pass
	// can be reached: both lists go through one method and the first one
	// would otherwise always be the failure.
	after int
	seen  int
}

func (f *failingResolver) Resolve(
	_ context.Context, _ config.Factory, _, _ string, deps map[string]config.DependencySpec,
) (map[string]string, []string, error) {
	f.seen++
	if f.seen <= f.after {
		out := map[string]string{}
		for name, d := range deps {
			out[name] = d.Version
		}

		return out, nil, nil
	}

	return nil, nil, f.err
}

func (f *failingResolver) ResolveTool(
	context.Context, config.Factory, string, string,
) (string, []string, error) {
	return "", nil, f.err
}

func (f *failingResolver) ResolveMembers(
	context.Context, config.Factory, string, string,
) (map[string]string, error) {
	return map[string]string{}, nil
}

// identityAnswers covers the pre-sync identity probe, which runs before the
// envrc pass and is not the subject of any of these.
func (h *harness) identityAnswers() {
	h.repos.EXPECT().Identity(mock.Anything).
		Return(map[string]string{"module": "example.com/g"}, nil).Maybe()
}

func TestSyncNamesWhichListRefused(t *testing.T) {
	t.Parallel()

	const withDev = factory + `devDependencies:
  go:
    example.com/tool: v1.0.0
`

	for name, tc := range map[string]struct {
		raw   string
		after int
		want  string
	}{
		"dependencies":    {factory, 0, "resolving go dependencies"},
		"devDependencies": {withDev, 1, "resolving go devDependencies"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.envrcExists(true)
			h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner,
				&failingResolver{err: errors.New("the register refused"), after: tc.after})
			h.identityAnswers()
			h.caller.EXPECT().Call(mock.Anything, mock.Anything, "language", mock.Anything, mock.Anything).
				RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
					return json.Unmarshal([]byte(`{"language":"go"}`), out)
				}).Maybe()

			_, err := h.c.Sync(t.Context(), parse(t, tc.raw), "/w", "")
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "the register refused")
		})
	}
}

func TestSyncNamesTheToolchainBinaryThatRefused(t *testing.T) {
	t.Parallel()

	const withTool = factory + `register:
  url: git@github.com:example/golden-register.git
toolchain:
  binaries:
    - name: a-tool
      module: example.com/a-tool
      track: go:example.com/a-tool
`

	h := newHarness(t)
	h.identityAnswers()
	h.envrcExists(true)
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner,
		&failingResolver{err: errors.New("the register refused")})

	_, err := h.c.Sync(t.Context(), parse(t, withTool), "/w", "")
	require.ErrorContains(t, err, "resolving toolchain binary a-tool")
}

func TestSyncStopsWhenAnEnvrcCannotBeWritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.identityAnswers()
	h.fs.EXPECT().Exists("/w/golden-go/.envrc").Return(false, nil).Once()
	h.fs.EXPECT().WriteFile("/w/golden-go/.envrc", mock.Anything).
		Return(errors.New("read only")).Once()

	// The PATH line is what puts the pinned store ahead of whatever the user
	// installed. Carrying on without it would sync a workspace that then
	// builds with the wrong tools, which is worse than not syncing.
	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorContains(t, err, "creating /w/golden-go/.envrc")
}

func TestSyncStopsWhenAnEnvrcCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.identityAnswers()
	h.fs.EXPECT().Exists("/w/golden-go/.envrc").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/golden-go/.envrc").Return(nil, errors.New("read only")).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorContains(t, err, "reading /w/golden-go/.envrc")
}

func TestSyncStopsWhenAnEnvrcCannotBeExtended(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.identityAnswers()
	h.fs.EXPECT().Exists("/w/golden-go/.envrc").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/golden-go/.envrc").
		Return([]byte("export FOO=1\n"), nil).Once()
	h.fs.EXPECT().WriteFile("/w/golden-go/.envrc", mock.Anything).
		Return(errors.New("read only")).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorContains(t, err, "extending /w/golden-go/.envrc")
}

// memberResolver stands in for a register carrying internal tracks: one
// module a member requires, one it does not, and one the factory declares.
type memberResolver struct{ passthrough }

func (memberResolver) ResolveMembers(
	_ context.Context, _ config.Factory, _, language string,
) (map[string]string, error) {
	if language != "go" {
		return map[string]string{}, nil
	}

	return map[string]string{
		"example.com/shared-spec": "v0.3.0",
		// A runnable member's own track. Its version is a pipeline dev
		// label: a real revision, and not a tag any proxy can fetch.
		"example.com/some-runnable": "v0.1.0-dev.r00000021.gabcdef012345",
		// Declared in the factory too.
		"sigs.k8s.io/yaml": "v9.9.9",
	}, nil
}

func (h *harness) memberRequires(modules ...string) {
	body := "module example.com/g\n\ngo 1.26\n\nrequire (\n"
	for _, m := range modules {
		body += "\t" + m + " v0.0.1\n"
	}

	body += ")\n"

	h.goMods["/w/golden-go/go.mod"] = body
}

func TestAMemberModuleIsPinnedByTheRegister(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.identityAnswers()
	// The member requires the declared module too, so only the declared-wins
	// rule can protect it here - not the requires filter.
	h.memberRequires("example.com/shared-spec", "sigs.k8s.io/yaml")
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner, memberResolver{})
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})

	var seen map[string]any

	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "render", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &seen))

			return json.Unmarshal([]byte(`{"files":[]}`), out)
		}).Once()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	deps, ok := seen["dependencies"].(map[string]any)
	require.True(t, ok)

	// The shared spec reaches the manifest, so the tidy that follows has a
	// version to keep rather than a tag to guess at.
	require.Equal(t, "v0.3.0", deps["example.com/shared-spec"])

	// The runnable's dev label does not. No member requires it, and a
	// require has to resolve before tidy can prune it as unused - so
	// writing it would fail the sync rather than govern anything.
	require.NotContains(t, deps, "example.com/some-runnable")

	// Naming a module in the factory is a deliberate act. The internal
	// track must not quietly override it.
	require.Equal(t, "v1.6.0", deps["sigs.k8s.io/yaml"])

	// And the operator is told, because a version they did not write just
	// appeared in a generated file.
	require.Contains(t, strings.Join(report.Notes, "\n"),
		"go:example.com/shared-spec pinned to v0.3.0 by the register's internal track")
}

// TestAReplaceBlockIsNotARequire drives the discriminator that keeps the
// requires filter safe.
//
// go.mod is not line-oriented. A module path sitting inside a replace or
// exclude block reads exactly like a require to a scanner that does not
// follow the open directive, and the answer is merged into one per-language
// map rendered into every member's manifest. So one replace block in one
// member would write a pipeline dev label into all of them, and every
// following tidy would fail on a version no proxy can serve. Six manifests
// broke that way once.
func TestAReplaceBlockIsNotARequire(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.identityAnswers()
	h.goMods["/w/golden-go/go.mod"] = `module example.com/g

go 1.26

require (
	example.com/shared-spec v0.0.1
)

replace (
	example.com/some-runnable => ../some-runnable
)

exclude (
	sigs.k8s.io/yaml v0.0.1
)
`
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner, memberResolver{})
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})

	var seen map[string]any

	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "render", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &seen))

			return json.Unmarshal([]byte(`{"files":[]}`), out)
		}).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	deps, ok := seen["dependencies"].(map[string]any)
	require.True(t, ok)

	require.NotContains(t, deps, "example.com/some-runnable",
		"a replace block names a module; it does not require one")

	// The require block still reads, so the fix narrows nothing it should
	// not: a directive-blind scanner and this one agree on a real require.
	require.Equal(t, "v0.3.0", deps["example.com/shared-spec"])
}

func TestSyncNamesAMemberModuleThatRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.identityAnswers()
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner,
		&refusingMembers{err: errors.New("the register refused")})
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})

	// Four lists resolve through this controller and they must not be
	// confusable in the error: a package name with no clue which one it
	// came from is what this wrapper exists to prevent.
	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorContains(t, err, "resolving go member modules")
	require.ErrorContains(t, err, "the register refused")
}

type refusingMembers struct {
	passthrough

	err error
}

func (r *refusingMembers) ResolveMembers(
	context.Context, config.Factory, string, string,
) (map[string]string, error) {
	return nil, r.err
}

// A member with no committed manifest yet requires nothing, which is what a
// repo cloned for the first time actually is.
func TestAMemberWithNoManifestRequiresNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(true)
	h.identityAnswers()
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner, memberResolver{})
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})

	var seen map[string]any

	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "render", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &seen))

			return json.Unmarshal([]byte(`{"files":[]}`), out)
		}).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	deps, ok := seen["dependencies"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, deps, "example.com/shared-spec")
}
