package synccontroller_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runtimecontroller"
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

type passthrough struct{}

func (passthrough) Resolve(_ context.Context, _ config.Factory, _, _ string, deps map[string]config.DependencySpec) (map[string]string, []string, error) {
	out := make(map[string]string, len(deps))
	for name, d := range deps {
		out[name] = d.Version
	}

	return out, nil, nil
}

func (passthrough) ResolveTool(context.Context, config.Factory, string, string) (string, []string, error) {
	return "v9.9.9", []string{"tool note"}, nil
}

func (passthrough) ResolveMembers(
	context.Context, config.Factory, string, string,
) (map[string]string, error) {
	return map[string]string{}, nil
}

type harness struct {
	caller           *engineadaptermock.MockCaller
	fs               *fsadaptermock.MockFS
	repos            *repoadaptermock.MockReader
	runner           *execadaptermock.MockRunner
	c                *synccontroller.Controller
	wrote            map[string]string
	gitignoreExists  bool
	gitignoreContent string
	gitignoreErr     error
	goMods           map[string]string
	registry         []byte
	record           []byte
	recordWriteErr   error
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

	h.fs.EXPECT().ReadFile(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/go.mod")
	})).RunAndReturn(func(path string) ([]byte, error) {
		if body, ok := h.goMods[path]; ok {
			return []byte(body), nil
		}

		return nil, errors.New("no manifest yet")
	}).Maybe()

	isRecord := func(path string) bool {
		return strings.HasSuffix(path, "/.forge/factory-generated.json")
	}

	h.fs.EXPECT().Exists(mock.MatchedBy(isRecord)).
		RunAndReturn(func(string) (bool, error) { return h.record != nil, nil }).Maybe()
	h.fs.EXPECT().ReadFile(mock.MatchedBy(isRecord)).RunAndReturn(func(string) ([]byte, error) {
		if h.record != nil {
			return h.record, nil
		}

		return nil, errors.New("no manifest yet")
	}).Maybe()
	h.fs.EXPECT().ReadFile(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/.forge/engines.yaml")
	})).RunAndReturn(func(string) ([]byte, error) {
		if h.registry != nil {
			return h.registry, nil
		}

		return nil, errors.New("no registry yet")
	}).Maybe()
	h.fs.EXPECT().WriteFile(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/.forge/engines.yaml")
	}), mock.Anything).RunAndReturn(func(path string, data []byte) error {
		h.wrote[path] = string(data)

		return nil
	}).Maybe()
	h.fs.EXPECT().MkdirAll(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/.forge")
	})).Return(nil).Maybe()
	h.fs.EXPECT().WriteFile(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/.forge/factory-generated.json")
	}), mock.Anything).RunAndReturn(func(path string, data []byte) error {
		if h.recordWriteErr != nil {
			return h.recordWriteErr
		}

		h.wrote[path] = string(data)

		return nil
	}).Maybe()

	isGitignore := func(path string) bool {
		return strings.HasSuffix(path, "/.gitignore")
	}

	h.fs.EXPECT().Exists(mock.MatchedBy(isGitignore)).
		RunAndReturn(func(string) (bool, error) { return h.gitignoreExists, h.gitignoreErr }).Maybe()
	h.fs.EXPECT().ReadFile(mock.MatchedBy(isGitignore)).
		RunAndReturn(func(string) ([]byte, error) { return []byte(h.gitignoreContent), nil }).Maybe()
	h.fs.EXPECT().WriteFile(mock.MatchedBy(isGitignore), mock.Anything).
		RunAndReturn(func(path string, data []byte) error {
			h.wrote[path] = string(data)

			return nil
		}).Maybe()

	return h
}

func (h *harness) gitignore(exists bool, content string) {
	h.gitignoreExists = exists
	h.gitignoreContent = content
}

func (h *harness) envrcExists(t *testing.T, exists bool) {
	isEnvrc := func(path string) bool {
		return strings.HasSuffix(path, "/.envrc")
	}

	h.fs.EXPECT().Exists(mock.MatchedBy(isEnvrc)).Return(exists, nil).Maybe()

	if exists {
		h.fs.EXPECT().ReadFile(mock.MatchedBy(isEnvrc)).
			Return([]byte(managedEnvrc(t)), nil).
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
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "module example.com/g\n", "gitignore": "golden-go"},
		{"path": "/w/go.work", "content": "use ./golden-go\n"},
	}})
	h.recordWrites()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	assert.Equal(t,
		[]string{".forge/engines.yaml", "/w/.forge/factory-generated.json", "/w/go.work", "/w/golden-go/go.mod"},
		report.Written)
	assert.Equal(t, []string{"/w/golden-go/.gitignore"}, report.Ignored)
	assert.Equal(t, "module example.com/g\n", h.wrote["/w/golden-go/go.mod"])
	assert.Contains(t, h.wrote["/w/golden-go/.gitignore"], "/go.mod")
	assert.Contains(t, h.wrote["/w/golden-go/.gitignore"], "forge-factory materialises these")
}

func TestSyncSendsTheDependenciesAndIdentityToTheEngine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
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
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "rust"})

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, synccontroller.ErrLanguage)
	assert.Contains(t, err.Error(), `registered as "go" but speaks "rust"`)
}

func TestSyncAddsToAGitignoreWithoutDisturbingIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.gitignore(true, ".envrc\ntmp/")
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
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.gitignore(true, "/go.mod\n/.envrc\n")
	h.fs.EXPECT().WriteFile("/w/golden-go/go.mod", mock.Anything).Return(nil).Once()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Empty(t, report.Ignored)
}

func TestSyncFailsWhenARepoCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(nil, assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
}

func TestSyncFailsWhenAnEngineFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
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
	h.envrcExists(t, true)
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
	h.envrcExists(t, true)
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
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "x", "gitignore": "golden-go"},
	}})
	h.fs.EXPECT().WriteFile("/w/golden-go/go.mod", mock.Anything).Return(nil).Once()
	h.gitignoreErr = assert.AnError

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
}

func TestSyncRefusesALanguageWithNoEngine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
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
	h.envrcExists(t, true)
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

func TestSyncNeverRunsTheDependencyLockCommands(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files": []map[string]any{{"path": "/w/golden-go/go.mod", "content": "x"}},
		"dependencyLock": []map[string]any{{
			"dir": "/w/golden-go", "command": "go", "args": []string{"mod", "tidy"},
			"env": map[string]string{"GOWORK": "off"}, "optional": true,
		}},
	})
	h.recordWrites()

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Empty(t, report.Locked)
	assert.Empty(t, report.Unlocked)
}

func TestLockRunsWhatAnEngineDeclares(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files": []map[string]any{{"path": "/w/golden-go/go.mod", "content": "x"}},
		"dependencyLock": []map[string]any{{
			"dir": "/w/golden-go", "command": "go", "args": []string{"mod", "tidy"},
			"env": map[string]string{"GOWORK": "off"}, "optional": true,
		}},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, "/w/golden-go", map[string]string{"GOWORK": "off"},
		"go", "mod", "tidy").Return(execadapter.Result{}, nil).Once()
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(false, nil).Once()
	h.recordWrites()

	report, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"go mod tidy in /w/golden-go"}, report.Locked)
	assert.Empty(t, report.Unlocked)
	assert.NotContains(t, h.wrote, "/w/golden-go/go.mod",
		"a lock writes no manifest; those are sync's business")

	assert.Equal(t,
		[]string{"/w/.forge/factory-generated.json", "/w/.forge/dependency-locks.json"},
		report.Written)
	assert.JSONEq(t, `{"version":1,"locks":[]}`, h.wrote["/w/.forge/dependency-locks.json"],
		"no lockfile was declared, so the manifest records none")
}

func TestLockRecordsTheLockfilesItResolved(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":     []any{},
		"lockfiles": []string{"/w/golden-go/go.sum", "/w/golden-go/absent.sum"},
	})

	isSum := func(path string) bool { return strings.HasSuffix(path, "/go.sum") }
	h.fs.EXPECT().Exists(mock.MatchedBy(isSum)).Return(true, nil).Once()
	h.fs.EXPECT().ReadFile(mock.MatchedBy(isSum)).Return([]byte("locked bytes\n"), nil).Once()
	h.fs.EXPECT().Exists("/w/golden-go/absent.sum").Return(false, nil).Once()
	h.recordWrites()

	report, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	sum := sha256.Sum256([]byte("locked bytes\n"))
	assert.JSONEq(t,
		fmt.Sprintf(`{"version":1,"locks":[{"path":"golden-go/go.sum","sha256":"%x"}]}`, sum),
		h.wrote["/w/.forge/dependency-locks.json"])

	require.NotEmpty(t, report.Notes)
	assert.Contains(t, strings.Join(report.Notes, "\n"), "no lockfile at golden-go/absent.sum")
}

func TestALockOverAMistakenEngineFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "rust"})

	_, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, synccontroller.ErrLanguage)
}

func TestAFailingLockfileReadFailsTheLock(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":     []any{},
		"lockfiles": []string{"/w/golden-go/go.sum"},
	})

	isSum := func(path string) bool { return strings.HasSuffix(path, "/go.sum") }
	h.fs.EXPECT().Exists(mock.MatchedBy(isSum)).Return(true, nil).Once()
	h.fs.EXPECT().ReadFile(mock.MatchedBy(isSum)).Return(nil, assert.AnError).Once()

	_, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "recording lockfile")
}

func TestAFailingManifestWriteFailsTheLock(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []any{}})
	h.fs.EXPECT().WriteFile("/w/.forge/dependency-locks.json", mock.Anything).
		Return(assert.AnError).Once()

	_, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "recording lockfiles")
}

func TestAnOptionalCommandThatFailsIsReportedAndTheLockStillPasses(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files": []any{},
		"dependencyLock": []map[string]any{
			{"dir": "/w/golden-go", "command": "go", "args": []string{"mod", "tidy"}, "optional": true},
		},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, mock.Anything, mock.Anything, "go", "mod", "tidy").
		Return(execadapter.Result{}, assert.AnError).Once()
	h.recordWrites()

	report, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err, "an optional failure is the caller's decision, not this controller's")
	require.Len(t, report.Unlocked, 1)
	assert.Contains(t, report.Unlocked[0], "go mod tidy in /w/golden-go")
}

func TestACommandThatIsNotOptionalStopsTheLock(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":          []any{},
		"dependencyLock": []map[string]any{{"dir": "/w", "command": "false"}},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, "/w", mock.Anything, "false").
		Return(execadapter.Result{}, assert.AnError).Once()

	_, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "running false")
}

func TestACommandThatExitsNonZeroCountsAsAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":          []any{},
		"dependencyLock": []map[string]any{{"dir": "/w", "command": "go", "args": []string{"mod", "tidy"}}},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, "/w", mock.Anything, "go", "mod", "tidy").
		Return(execadapter.Result{ExitCode: 1, Stderr: "unknown directive: #"}, nil).Once()

	_, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, synccontroller.ErrCommand,
		"a non zero exit comes back with no error, so reading only the error passes every failure")
	assert.Contains(t, err.Error(), "unknown directive")
}

func TestALongFailureIsTrimmedToItsReason(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":          []any{},
		"dependencyLock": []map[string]any{{"dir": "/w", "command": "go"}},
	})
	h.runner.EXPECT().RunEnv(mock.Anything, "/w", mock.Anything, "go").
		Return(execadapter.Result{
			ExitCode: 2,
			Stderr:   strings.Repeat("x", 600) + "the reason",
		}, nil).Once()

	_, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the reason")
	assert.Less(t, len(err.Error()), 600)
}

func TestAFileCanNameMoreThanItselfToIgnore(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{{
		"path": "/w/golden-go/go.mod", "content": "x",
		"gitignore": "golden-go", "alsoIgnore": []string{"go.sum"},
	}}})
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

func TestARunCacheSyncWhoseWorkspaceIsOneMemberRendersThatMemberAndTheRoot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files": []map[string]any{
			{"path": "/w/golden-go/go.mod", "content": "module example.com/g\n"},
			{"path": "/w/other-go/go.mod", "content": "module example.com/o\n"},
			{"path": "/w/go.work", "content": "use ./golden-go\n"},
		},
		"dependencyLock": []map[string]any{
			{"dir": "/w/golden-go", "command": "true"},
			{"dir": "/w/other-go", "command": "false"},
			{"dir": "/w", "command": "true"},
		},
	})
	h.recordWrites()

	report, err := h.c.Sync(t.Context(), parse(t, twoMemberFactory), "/w", "golden-go")
	require.NoError(t, err)

	assert.Equal(t,
		[]string{".forge/engines.yaml", "/w/.forge/factory-generated.json", "/w/go.work", "/w/golden-go/go.mod"},
		report.Written,
		"the one member and the root land; the absent member never does")
	assert.NotContains(t, h.wrote, "/w/other-go/go.mod")
}

func TestARunCacheLockWhoseWorkspaceIsOneMemberRunsThatMemberAndTheRoot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files": []any{},
		"dependencyLock": []map[string]any{
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

	report, err := h.c.Lock(t.Context(), parse(t, twoMemberFactory), "/w", "golden-go")
	require.NoError(t, err)
	assert.Len(t, report.Locked, 2)
}

func TestSyncResolvesTheToolchainBinaries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
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

func TestSyncResolvesTheDeclaredRuntimes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	f := config.Factory{
		Repos: []config.Repo{{Name: "member-a"}},
		Toolchain: &config.Toolchain{Runtimes: map[string]config.RuntimeSpec{
			"jre":  {Version: "21.0.5+11", Params: map[string]string{"source": "temurin"}},
			"go":   {Version: "1.26.5"},
			"rust": {Track: "runtime:rust"},
		}},
	}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	require.Equal(t, []runtimecontroller.Pin{
		{Name: "go", Version: "1.26.5"},
		{Name: "jre", Version: "21.0.5+11", Params: map[string]string{"source": "temurin"}},
		{Name: "rust", Version: "v9.9.9"},
	}, report.Runtimes)
	require.Contains(t, report.Notes, "tool note")
}

func TestTheRuntimeSpecTargetsReachThePin(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	f := config.Factory{
		Repos: []config.Repo{{Name: "member-a"}},
		Toolchain: &config.Toolchain{Runtimes: map[string]config.RuntimeSpec{
			"rust": {Version: "1.97.0", Targets: []string{"x86_64-pc-windows-gnu", "aarch64-unknown-linux-gnu"}},
		}},
	}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	require.Equal(t, []runtimecontroller.Pin{{
		Name: "rust", Version: "1.97.0",
		Targets: []string{"x86_64-pc-windows-gnu", "aarch64-unknown-linux-gnu"},
	}}, report.Runtimes)
}

func TestSyncResolvesAndPersistsTheToolchainImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()
	h.fs.EXPECT().ReadFile("/w/.forge/toolchain-image").Return(nil, assert.AnError).Once()
	h.recordWrites()

	f := config.Factory{
		Repos: []config.Repo{{Name: "member-a"}},
		Toolchain: &config.Toolchain{Image: &config.ToolchainImage{
			Ref: "ghcr.io/owner/tools", Track: "internal:ghcr.io/owner/tools",
		}},
	}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	assert.Equal(t, "ghcr.io/owner/tools:v9.9.9", report.Image,
		"the track resolves through the register like any dependency")
	assert.Equal(t, "ghcr.io/owner/tools:v9.9.9\n", h.wrote["/w/.forge/toolchain-image"])
	assert.Contains(t, report.Written, ".forge/toolchain-image")
	assert.Contains(t, report.Notes, "tool note")
}

func TestSyncLeavesAConvergedToolchainImageAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()
	h.fs.EXPECT().ReadFile("/w/.forge/toolchain-image").
		Return([]byte("ghcr.io/owner/tools:v1.0.0\n"), nil).Once()

	f := config.Factory{
		Repos: []config.Repo{{Name: "member-a"}},
		Toolchain: &config.Toolchain{Image: &config.ToolchainImage{
			Ref: "ghcr.io/owner/tools", Version: "v1.0.0",
		}},
	}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	assert.Equal(t, "ghcr.io/owner/tools:v1.0.0", report.Image,
		"a literal pin is written verbatim, register not consulted")
	assert.NotContains(t, report.Written, ".forge/toolchain-image")
}

func TestSyncWritesTheFactoryEnginesAsTheOuterRegistryRing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()
	h.recordWrites()

	f := config.Factory{
		Repos: []config.Repo{{Name: "member-a"}},
		Engines: []config.Engine{
			{Alias: "go", Engine: "forge://github.com/owner/forge-factory/cmd/factory-lang-go"},
			{Alias: "lint", Engine: "forge://github.com/owner/tools/cmd/lint"},
		},
	}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	assert.Contains(t, report.Written, ".forge/engines.yaml")
	assert.Equal(t, "# Generated by forge-factory sync. DO NOT EDIT.\n"+
		"# The factory's engines by alias: the outer ring of every member's engine\n"+
		"# registry. A member's own forge.yaml entry wins over one here.\n"+
		"engines:\n"+
		"  - alias: go\n    engine: forge://github.com/owner/forge-factory/cmd/factory-lang-go\n"+
		"  - alias: lint\n    engine: forge://github.com/owner/tools/cmd/lint\n",
		h.wrote["/w/.forge/engines.yaml"])
}

func TestSyncLeavesAConvergedEngineRegistryAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	f := config.Factory{
		Repos:   []config.Repo{{Name: "member-a"}},
		Engines: []config.Engine{{Alias: "go", Engine: "forge://github.com/owner/forge-factory/cmd/factory-lang-go"}},
	}

	h.registry = synccontroller.RenderEngines(f.Engines)

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	assert.NotContains(t, report.Written, ".forge/engines.yaml")
}

func TestSyncCreatesAMissingEnvrc(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, false)
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}, {Name: "member-b"}}}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	require.Equal(t, managedEnvrc(t), h.wrote["/w/member-a/.envrc"])
	require.Contains(t, report.Written, "/w/member-a/.envrc")
	require.Contains(t, report.Written, "/w/member-b/.envrc")
}

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
		managedEnvrc(t)+"export MY_OWN=1\n",
		h.wrote["/w/member-a/.envrc"])
	require.Contains(t, report.Written, "/w/member-a/.envrc")
}

type failingResolver struct {
	err   error
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
			h.envrcExists(t, true)
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
	h.envrcExists(t, true)
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner,
		&failingResolver{err: errors.New("the register refused")})

	_, err := h.c.Sync(t.Context(), parse(t, withTool), "/w", "")
	require.ErrorContains(t, err, "resolving toolchain binary a-tool")
}

func TestSyncNamesAToolchainImageThatCannotResolve(t *testing.T) {
	t.Parallel()

	const withImage = factory + `register:
  url: git@github.com:example/golden-register.git
toolchain:
  image:
    ref: ghcr.io/owner/tools
    track: internal:ghcr.io/owner/tools
`

	h := newHarness(t)
	h.identityAnswers()
	h.envrcExists(t, true)
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner,
		&failingResolver{err: errors.New("the register refused")})

	_, err := h.c.Sync(t.Context(), parse(t, withImage), "/w", "")
	require.ErrorContains(t, err, "resolving the toolchain image")
}

func TestSyncStopsWhenAToolchainImageCannotBePersisted(t *testing.T) {
	t.Parallel()

	const withImage = factory + `toolchain:
  image:
    ref: ghcr.io/owner/tools
    version: v1.0.0
`

	h := newHarness(t)
	h.identityAnswers()
	h.envrcExists(t, true)
	h.fs.EXPECT().ReadFile("/w/.forge/toolchain-image").Return(nil, assert.AnError).Once()
	h.fs.EXPECT().WriteFile("/w/.forge/toolchain-image", mock.Anything).
		Return(errors.New("read only")).Once()

	_, err := h.c.Sync(t.Context(), parse(t, withImage), "/w", "")
	require.ErrorContains(t, err, "persisting the toolchain image")
}

func TestSyncStopsWhenAnEnvrcCannotBeWritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.identityAnswers()
	h.fs.EXPECT().Exists("/w/golden-go/.envrc").Return(false, nil).Once()
	h.fs.EXPECT().WriteFile("/w/golden-go/.envrc", mock.Anything).
		Return(errors.New("read only")).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorContains(t, err, "writing /w/golden-go/.envrc")
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
	require.ErrorContains(t, err, "writing /w/golden-go/.envrc")
}

type memberResolver struct{ passthrough }

func (memberResolver) ResolveMembers(
	_ context.Context, _ config.Factory, _, language string,
) (map[string]string, error) {
	if language != "go" {
		return map[string]string{}, nil
	}

	return map[string]string{
		"example.com/shared-spec":   "v0.3.0",
		"example.com/some-runnable": "v0.1.0-dev.r00000021.gabcdef012345",
		"sigs.k8s.io/yaml":          "v9.9.9",
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
	h.envrcExists(t, true)
	h.identityAnswers()
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

	require.Equal(t, "v0.3.0", deps["example.com/shared-spec"])
	require.NotContains(t, deps, "example.com/some-runnable")
	require.Equal(t, "v1.6.0", deps["sigs.k8s.io/yaml"])
	require.Contains(t, strings.Join(report.Notes, "\n"),
		"go:example.com/shared-spec pinned to v0.3.0 by the register's internal track")
}

func TestAReplaceBlockIsNotARequire(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
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
	require.Equal(t, "v0.3.0", deps["example.com/shared-spec"])
}

func TestSyncNamesAMemberModuleThatRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.identityAnswers()
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner,
		&refusingMembers{err: errors.New("the register refused")})
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})

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

func TestAMemberWithNoManifestRequiresNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
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

func managedEnvrc(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs("/w")
	require.NoError(t, err)

	env := filepath.Join(abs, ".forge", "env")

	return fmt.Sprintf("# BEGIN forge-factory\nexport PATH=\"%s:$PATH\"\nif [ -f %q ]; then . %q; fi\n# END forge-factory\n",
		filepath.Join(abs, ".forge", "bin"), env, env)
}

func TestTheManagedEnvrcSourcesCleanlyWithNoEnvFile(t *testing.T) {
	t.Parallel()

	envrc := filepath.Join(t.TempDir(), ".envrc")
	require.NoError(t, os.WriteFile(envrc, []byte(managedEnvrc(t)), 0o600))

	out, err := exec.Command("sh", "-e", "-c", ". "+envrc+" && echo sourced").CombinedOutput()
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "sourced")
}

func TestAMovedWorkspaceRewritesTheManagedBlock(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()

	stale := "# BEGIN forge-factory\nexport PATH=\"/somewhere/else/.forge/bin:$PATH\"\n" +
		"# END forge-factory\nexport MINE=1\n"
	h.envrcContent(t, stale)

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}}}

	_, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	got := h.wrote["/w/member-a/.envrc"]
	assert.Contains(t, got, "/w/.forge/bin",
		"the block carries this workspace's path, not the one it was cloned from")
	assert.NotContains(t, got, "/somewhere/else",
		"and the stale path is gone rather than sitting above the new one")
	assert.Contains(t, got, "export MINE=1",
		"everything outside the markers is the machine's own")
}

func TestAnEnvrcWithNoMarkersKeepsWhatItHad(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()
	h.envrcContent(t, "export SECRET=shhh\n")

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}}}

	_, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	got := h.wrote["/w/member-a/.envrc"]
	assert.Contains(t, got, "export SECRET=shhh")
	assert.True(t, strings.HasPrefix(got, "# BEGIN forge-factory"),
		"prepended, so the workspace tooling wins over what the machine adds later")
}

func TestAMentionOfForgeBinInACommentDoesNotSuppressTheBlock(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()
	h.envrcContent(t, "# forge puts things in /.forge/bin, apparently\n")

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}}}

	_, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	assert.Contains(t, h.wrote["/w/member-a/.envrc"], "# BEGIN forge-factory")
}

func TestAConvergedEnvrcIsNotRewritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()
	h.envrcContent(t, managedEnvrc(t)+"export MINE=1\n")

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}}}

	report, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	assert.NotContains(t, report.Written, "/w/member-a/.envrc")
}

func TestEveryMemberIgnoresTheEnvrcSyncWrites(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.recordWrites()
	h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()
	h.envrcContent(t, managedEnvrc(t))

	f := config.Factory{Repos: []config.Repo{{Name: "member-a"}, {Name: "member-b"}}}

	_, err := h.c.Sync(context.Background(), f, "/w", "")
	require.NoError(t, err)

	assert.Contains(t, h.wrote["/w/member-a/.gitignore"], "/.envrc")
	assert.Contains(t, h.wrote["/w/member-b/.gitignore"], "/.envrc")
}

func (h *harness) envrcContent(t *testing.T, body string) {
	t.Helper()

	isEnvrc := func(path string) bool { return strings.HasSuffix(path, "/.envrc") }

	h.fs.EXPECT().Exists(mock.MatchedBy(isEnvrc)).Return(true, nil).Maybe()
	h.fs.EXPECT().ReadFile(mock.MatchedBy(isEnvrc)).Return([]byte(body), nil).Maybe()
}

func TestARepeatedGeneratedPathIsRecordedOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": "module example.com/g\n"},
		{"path": "/w/golden-go/go.mod", "content": "module example.com/g\n"},
	}})
	h.recordWrites()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)

	assert.JSONEq(t, `{"version":1,"files":[{"path":"golden-go/go.mod","sha256":"`+
		synccontroller.Digest([]byte("module example.com/g\n"))+`"}]}`,
		h.wrote["/w/.forge/factory-generated.json"])
}

func answersOnce(t *testing.T, caller *engineadaptermock.MockCaller, payload any) {
	t.Helper()

	caller.EXPECT().Call(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			raw, err := json.Marshal(payload)
			if err != nil {
				return err
			}

			return json.Unmarshal(raw, out)
		}).Once()
}

func TestAFailedGeneratedRecordFailsTheLock(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	fs := fsadaptermock.NewMockFS(t)
	repos := repoadaptermock.NewMockReader(t)
	c := synccontroller.New(caller, fs,
		repos, execadaptermock.NewMockRunner(t), passthrough{})

	repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	answersOnce(t, caller, map[string]any{"language": "go"})
	answersOnce(t, caller, map[string]any{"files": []any{}})
	fs.EXPECT().ReadFile(mock.Anything).Return(nil, errors.New("absent")).Maybe()
	fs.EXPECT().Exists(mock.Anything).Return(false, nil).Maybe()
	fs.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(assert.AnError).Once()

	_, err := c.Lock(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "recording the render of /w")
}

func TestAFailedGeneratedRecordFailsTheSync(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	fs := fsadaptermock.NewMockFS(t)
	repos := repoadaptermock.NewMockReader(t)
	c := synccontroller.New(caller, fs,
		repos, execadaptermock.NewMockRunner(t), passthrough{})

	repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
	answersOnce(t, caller, map[string]any{"language": "go"})
	answersOnce(t, caller, map[string]any{"files": []any{}})
	fs.EXPECT().ReadFile(mock.Anything).Return(nil, errors.New("absent")).Maybe()
	fs.EXPECT().Exists(mock.Anything).Return(false, nil).Maybe()
	fs.EXPECT().WriteFile(mock.MatchedBy(func(path string) bool {
		return !strings.HasSuffix(path, "/.forge/factory-generated.json")
	}), mock.Anything).Return(nil).Maybe()
	fs.EXPECT().WriteFile(mock.MatchedBy(func(path string) bool {
		return strings.HasSuffix(path, "/.forge/factory-generated.json")
	}), mock.Anything).Return(assert.AnError).Once()

	_, err := c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "recording the render of /w")
}

const twoLanguageFactory = `version: "1"
name: golden
repos:
  - name: golden-go
    url: u
    languages: [go]
  - name: golden-rust
    url: v
    languages: [rust]
register:
  url: git@github.com:example/golden-register.git
engines:
  - alias: go
    engine: forge://example.com/lang-go
  - alias: rust
    engine: forge://example.com/lang-rust
`

func registerRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "golden-register"), 0o750))

	return root
}

func syncAgainstARealRegister(t *testing.T, root, raw string, now func() time.Time) (*harness, error) {
	t.Helper()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.identityAnswers()
	h.recordWrites()
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner,
		resolvecontroller.New(fsadapter.New(), gitadapter.New(execadapter.New()), now))

	for uri, language := range map[string]string{
		"forge://example.com/lang-go":   "go",
		"forge://example.com/lang-rust": "rust",
	} {
		h.caller.EXPECT().Call(mock.Anything, uri, synccontroller.ToolLanguage, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
				return json.Unmarshal([]byte(`{"language":"`+language+`"}`), out)
			}).Maybe()
	}

	h.caller.EXPECT().Call(mock.Anything, mock.Anything, synccontroller.ToolRender, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			return json.Unmarshal([]byte(`{"files":[]}`), out)
		}).Maybe()

	_, err := h.c.Sync(t.Context(), parse(t, raw), root, "")

	return h, err
}

func TestASecondSyncFilesNoSecondRequestWhileTheFirstIsOpen(t *testing.T) {
	t.Parallel()

	root := registerRoot(t)
	first := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	_, err := syncAgainstARealRegister(t, root, oneMissingFactory, func() time.Time { return first })
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.ErrorContains(t, err, "filed go/example.com/missing-lib/")

	_, err = syncAgainstARealRegister(t, root, oneMissingFactory, func() time.Time { return first.Add(time.Hour) })
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.ErrorContains(t, err, "is already open")

	require.Len(t, filedRequests(t, root), 1)
}

const oneMissingFactory = twoLanguageFactory + `dependencies:
  go:
    example.com/missing-lib: {}
`

func filedRequests(t *testing.T, root string) []string {
	t.Helper()

	filed, err := filepath.Glob(filepath.Join(root, "golden-register", "request", "go", "example.com", "missing-lib", "*.json"))
	require.NoError(t, err)

	return filed
}

func TestAFiledRequestNamesTheFactoryAsItsRequester(t *testing.T) {
	t.Parallel()

	root := registerRoot(t)

	_, err := syncAgainstARealRegister(t, root, oneMissingFactory, time.Now)
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)

	filed := filedRequests(t, root)
	require.Len(t, filed, 1)

	request, err := os.ReadFile(filed[0])
	require.NoError(t, err)
	assert.Contains(t, string(request), `"requester":"golden"`)
}

func TestOneSyncNamesEveryMissingPackageInOneError(t *testing.T) {
	t.Parallel()

	const threeMissing = twoLanguageFactory + `dependencies:
  go:
    example.com/missing-lib: {}
    example.com/other-missing-lib: {}
  rust:
    missing-crate: {}
toolchain:
  runtimes:
    go: { track: "runtime:missing-runtime" }
  binaries:
    - name: missing-tool
      module: example.com/missing-tool
      track: go:example.com/missing-tool
  image:
    ref: ghcr.io/owner/tools
    track: internal:ghcr.io/owner/missing-image
`

	h, err := syncAgainstARealRegister(t, registerRoot(t), threeMissing, time.Now)
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.ErrorContains(t, err, "go:example.com/missing-lib is not in the register")
	require.ErrorContains(t, err, "go:example.com/other-missing-lib is not in the register")
	require.ErrorContains(t, err, "rust:missing-crate is not in the register")
	require.ErrorContains(t, err, "go:example.com/missing-tool is not in the register")
	require.ErrorContains(t, err, "runtime:missing-runtime is not in the register")
	require.ErrorContains(t, err, "internal:ghcr.io/owner/missing-image is not in the register")
	h.caller.AssertNotCalled(t, "Call", mock.Anything, mock.Anything, synccontroller.ToolRender, mock.Anything, mock.Anything)
}

type unregisteredIn struct {
	passthrough

	language string
}

func (u unregisteredIn) Resolve(
	ctx context.Context, f config.Factory, root, language string, deps map[string]config.DependencySpec,
) (map[string]string, []string, error) {
	if language == u.language {
		return nil, nil, fmt.Errorf("%s:missing-crate is %w", language, resolvecontroller.ErrUnregistered)
	}

	return u.passthrough.Resolve(ctx, f, root, language, deps)
}

func TestALanguageWithAMissingEntryRendersNothingWhileTheOthersRender(t *testing.T) {
	t.Parallel()

	cargo := synccontroller.RenderedFile{Path: "golden-rust/Cargo.toml", SHA256: synccontroller.Digest([]byte("[package]\n"))}

	h := newHarness(t)
	h.envrcExists(t, true)
	h.identityAnswers()
	h.record = recordDocument(t, cargo)
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner, unregisteredIn{language: "rust"})
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-rust", "language", map[string]any{"language": "rust"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": goMod},
	}})
	h.recordWrites()

	_, err := h.c.Sync(t.Context(), parse(t, twoLanguageFactory), "/w", "")
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.ErrorContains(t, err, "rust:missing-crate is not in the register")

	assert.Equal(t, goMod, h.wrote["/w/golden-go/go.mod"])
	assert.JSONEq(t, string(recordDocument(t,
		synccontroller.RenderedFile{Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte(goMod))},
		cargo,
	)), h.wrote["/w/.forge/factory-generated.json"],
		"the skipped language keeps its files in the record, so the next sync still guards them")
}

func TestALockWithAMissingPackageRunsNoCommandAndWritesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.identityAnswers()
	h.c = synccontroller.New(h.caller, h.fs, h.repos, h.runner, unregisteredIn{language: "rust"})
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-rust", "language", map[string]any{"language": "rust"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{
		"files":          []map[string]any{{"path": "/w/golden-go/go.mod", "content": goMod}},
		"dependencyLock": []map[string]any{{"dir": "/w/golden-go", "command": "true"}},
		"lockfiles":      []string{"/w/golden-go/go.sum"},
	})
	h.fs.EXPECT().Exists(mock.Anything).Return(true, nil).Maybe()
	h.goMods["/w/golden-go/go.mod"] = goMod
	h.recordWrites()

	_, err := h.c.Lock(t.Context(), parse(t, twoLanguageFactory), "/w", "")
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.ErrorContains(t, err, "rust:missing-crate is not in the register")
	h.runner.AssertNotCalled(t, "RunEnv", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	assert.Empty(t, h.wrote, "neither the render record nor the dependency locks may be written")
}

func lockWithRustSkipped(t *testing.T, root string, recorded ...synccontroller.RenderedFile) error {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(root, ".forge"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, filepath.FromSlash(synccontroller.GeneratedManifestPath)),
		recordDocument(t, recorded...), 0o600))

	caller := engineadaptermock.NewMockCaller(t)
	repos := repoadaptermock.NewMockReader(t)
	repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Maybe()
	answersFor(caller, "forge://example.com/lang-go", synccontroller.ToolLanguage, `{"language":"go"}`)
	answersFor(caller, "forge://example.com/lang-rust", synccontroller.ToolLanguage, `{"language":"rust"}`)
	answersFor(caller, "forge://example.com/lang-go", synccontroller.ToolRender, `{"files":[]}`)

	c := synccontroller.New(caller, fsadapter.New(), repos, execadaptermock.NewMockRunner(t), unregisteredIn{language: "rust"})

	_, err := c.Lock(t.Context(), parse(t, twoLanguageFactory), root, "")

	return err
}

func answersFor(caller *engineadaptermock.MockCaller, uri, tool, payload string) {
	caller.EXPECT().Call(mock.Anything, uri, tool, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			return json.Unmarshal([]byte(payload), out)
		}).Maybe()
}

func TestALockWithASkippedLanguageNamesTheMissingPackageAndNeverGuardsAFileNoLanguageRenders(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "FOLLOWUP.md"), []byte("edited by the user\n"), 0o600))

	err := lockWithRustSkipped(t, root, synccontroller.RenderedFile{
		Path: "FOLLOWUP.md", SHA256: synccontroller.Digest([]byte("as the bootstrap placed it\n")),
	})
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.NotErrorIs(t, err, synccontroller.ErrHandEdited)
}

func TestALockWithASkippedLanguageKeepsACarriedLockfileWithItsEmptyDigest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "golden-rust"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "golden-rust", "Cargo.lock"), []byte("locked\n"), 0o600))

	err := lockWithRustSkipped(t, root, synccontroller.RenderedFile{Path: "golden-rust/Cargo.lock"})
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)

	record, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(synccontroller.GeneratedManifestPath)))
	require.NoError(t, readErr)
	assert.JSONEq(t, string(recordDocument(t, synccontroller.RenderedFile{Path: "golden-rust/Cargo.lock"})), string(record))
}
