package synccontroller_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/engineadaptermock"
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
    engine: go://example.com/lang-go
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

type harness struct {
	caller *engineadaptermock.MockCaller
	fs     *fsadaptermock.MockFS
	repos  *repoadaptermock.MockReader
	c      *synccontroller.Controller
	wrote  map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		caller: engineadaptermock.NewMockCaller(t),
		fs:     fsadaptermock.NewMockFS(t),
		repos:  repoadaptermock.NewMockReader(t),
		wrote:  map[string]string{},
	}

	h.c = synccontroller.New(h.caller, h.fs, h.repos)

	return h
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
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("go://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("go://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "module example.com/g\n", "gitignore": "golden-go"},
		{"path": "/w/go.work", "content": "use ./golden-go\n"},
	}})
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(false, nil).Once()
	h.recordWrites()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
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
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("go://example.com/lang-go", "language", map[string]any{"language": "go"})

	var seen map[string]any

	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "render", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &seen))

			return json.Unmarshal([]byte(`{"files":[]}`), out)
		}).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
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
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("go://example.com/lang-go", "language", map[string]any{"language": "rust"})

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, synccontroller.ErrLanguage)
	assert.Contains(t, err.Error(), `registered as "go" but speaks "rust"`)
}

func TestSyncAddsToAGitignoreWithoutDisturbingIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("go://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("go://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/golden-go/.gitignore").Return([]byte(".envrc\ntmp/"), nil).Once()
	h.recordWrites()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)

	got := h.wrote["/w/golden-go/.gitignore"]
	assert.Contains(t, got, ".envrc")
	assert.Contains(t, got, "tmp/")
	assert.Contains(t, got, "/go.mod")
}

func TestSyncLeavesAGitignoreAloneWhenItAlreadyNamesEverything(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("go://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("go://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/golden-go/.gitignore").Return([]byte("/go.mod\n"), nil).Once()
	h.fs.EXPECT().WriteFile("/w/golden-go/go.mod", mock.Anything).Return(nil).Once()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)
	assert.Empty(t, report.Ignored)
}

func TestSyncFailsWhenARepoCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(nil, assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}

func TestSyncFailsWhenAnEngineFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("go://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "render", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "rendering go")
}

func TestSyncFailsWhenTheLanguageProbeFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "language", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "what language it speaks")
}

func TestSyncFailsWhenAFileCannotBeWritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("go://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("go://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/go.work", "content": "x"},
	}})
	h.fs.EXPECT().WriteFile("/w/go.work", mock.Anything).Return(assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}

func TestSyncFailsWhenAGitignoreCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("go://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("go://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.fs.EXPECT().WriteFile("/w/golden-go/go.mod", mock.Anything).Return(nil).Once()
	h.fs.EXPECT().Exists("/w/golden-go/.gitignore").Return(false, assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}

func TestSyncRefusesALanguageWithNoEngine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()

	f := parse(t, factory)
	f.Engines = nil

	_, err := h.c.Sync(t.Context(), f, "/w")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no engine is registered")
}

func TestTheLanguageProbeNeverSendsNull(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()

	var raw []byte

	h.caller.EXPECT().Call(mock.Anything, mock.Anything, "language", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			var err error

			raw, err = json.Marshal(in)
			require.NoError(t, err)

			return json.Unmarshal([]byte(`{"language":"go"}`), out)
		}).Once()
	h.answers("go://example.com/lang-go", "render", map[string]any{"files": []any{}})

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)

	assert.NotContains(t, string(raw), "null",
		"a nil slice or map travels as null and the engine's own schema refuses it")
}
