package synccontroller_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
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

type harness struct {
	caller *engineadaptermock.MockCaller
	fs     *fsadaptermock.MockFS
	repos  *repoadaptermock.MockReader
	runner *execadaptermock.MockRunner
	c      *synccontroller.Controller
	wrote  map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		caller: engineadaptermock.NewMockCaller(t),
		fs:     fsadaptermock.NewMockFS(t),
		repos:  repoadaptermock.NewMockReader(t),
		runner: execadaptermock.NewMockRunner(t),
		wrote:  map[string]string{},
	}

	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner, passthrough{})

	return h
}

// envrcExists answers the pre-sync existence probe for every member's
// .envrc; most tests want them present so no create is recorded.
func (h *harness) envrcExists(exists bool) {
	h.fs.EXPECT().Exists(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/.envrc")
	})).Return(exists, nil).Maybe()
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

// TestSyncCreatesAMissingEnvrc: forge sources a .envrc in every repo it
// builds, so a fresh workspace used to need a hand-run touch loop. Sync
// creates the missing file empty and never touches one that exists -
// its content is the machine's own.
func TestSyncCreatesAMissingEnvrc(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(false)
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}, {Name: "member-b"}}}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	require.Equal(t, "", h.wrote["/w/member-a/.envrc"])
	require.Contains(t, report.Written, "/w/member-a/.envrc")
	require.Contains(t, report.Written, "/w/member-b/.envrc")
}
