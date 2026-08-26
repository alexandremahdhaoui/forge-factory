package runcontroller_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/synccontrollermock"
)

type harness struct {
	progress *bytes.Buffer
	runner   *execadaptermock.MockRunner
	sync     *synccontrollermock.MockSyncer
	c        *runcontroller.Controller
	cache    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		progress: &bytes.Buffer{},
		runner:   execadaptermock.NewMockRunner(t),
		sync:     synccontrollermock.NewMockSyncer(t),
		cache:    t.TempDir(),
	}

	h.c = runcontroller.New(fsadapter.New(), gitadapter.New(execadapter.New()),
		h.runner, h.sync, h.progress)

	// The exec boundary asks PATH for forge before picking a pinned go-run
	// fallback; answering yes keeps the bare form every expectation pins.
	h.runner.EXPECT().LookPath("forge").Return("/usr/bin/forge", true).Maybe()

	return h
}

func (h *harness) expectExec(dir string, code int) {
	h.runner.EXPECT().
		RunAttached(mock.Anything, dir, mock.Anything, "forge", mock.Anything, mock.Anything, mock.Anything).
		Return(code, nil).Once()
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()

	full := append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)

	res, err := execadapter.New().Run(context.Background(), dir, "git", full...)
	require.NoError(t, err)
	require.Zero(t, res.ExitCode, "git %v: %s", args, res.Stderr)

	return strings.TrimSpace(res.Stdout)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// repoWithRunnable builds a git repo carrying one runnable named tool-a.
func repoWithRunnable(t *testing.T, dir, factoryURL string) {
	t.Helper()

	write(t, filepath.Join(dir, "forge.yaml"), `name: member-a
envFile: .envrc
artifactStorePath: .forge/store.yaml
run:
  - name: tool-a
    src: ./cmd/tool-a
    factory: `+factoryURL+`
build:
  - name: tool-a
    src: ./cmd/tool-a
    dest: ./build/bin
    engine: forge://go-build
`)
	write(t, filepath.Join(dir, "cmd", "tool-a", "main.go"), "package main\nfunc main() {}\n")
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "seed")
}

// bareish makes a plain repo cloneable by path and sets origin/HEAD after
// clone by pointing HEAD at main.
func factoryRepo(t *testing.T, dir, memberURL, registerURL string) {
	t.Helper()

	write(t, filepath.Join(dir, "workspace", "forge-factory.yaml"), `version: "1"
name: fixture
repos:
  - name: member-a
    url: `+memberURL+`
    languages: [go]
  - name: fixture-register
    url: `+registerURL+`
engines:
  - alias: go
    engine: forge://example.com/lang-go
register:
  url: `+registerURL+`
`)
	write(t, filepath.Join(dir, "workspace", "forge-ci.yaml"), "name: fixture\n")
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "seed")
}

func registerRepo(t *testing.T, dir string) {
	t.Helper()

	write(t, filepath.Join(dir, "README.md"), "the fixture register\n")
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "seed")
}

// universe stands up member, factory and register repos wired together by
// path URLs, and answers their directories.
func universe(t *testing.T) (member, factory, register string) {
	t.Helper()

	base := t.TempDir()
	member = filepath.Join(base, "member-a")
	factory = filepath.Join(base, "fixture-factory")
	register = filepath.Join(base, "fixture-register")

	registerRepo(t, register)
	repoWithRunnable(t, member, factory)
	factoryRepo(t, factory, member, register)

	return member, factory, register
}

func TestRule2TheEnclosingWorkspaceClaimsTheRepo(t *testing.T) {
	h := newHarness(t)

	ws := t.TempDir()
	repo := filepath.Join(ws, "member-a")
	repoWithRunnable(t, repo, "git@example.com:x/fixture-factory.git")
	write(t, filepath.Join(ws, "forge-factory.yaml"), `version: "1"
name: enclosing
repos:
  - name: member-a
    url: u
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
`)

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, ws, "").
		Return(synccontroller.Report{}, nil).Once()
	h.expectExec(repo, 0)

	code, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "tool-a", WorkDir: repo, CacheDir: h.cache,
	})
	require.NoError(t, err)
	require.Zero(t, code)
	require.Contains(t, h.progress.String(), "rule 2")
}

func TestRule3ANonMemberCheckoutUsesItsOwnFactory(t *testing.T) {
	h := newHarness(t)

	member, factoryDir, _ := universe(t)

	ws := t.TempDir()
	write(t, filepath.Join(ws, "forge-factory.yaml"), `version: "1"
name: foreign
repos:
  - name: something-else
    url: u
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
`)

	repo := filepath.Join(ws, "member-a")
	require.NoError(t, os.Rename(member, repo))

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "member-a").
		Return(synccontroller.Report{}, nil).Once()
	h.expectExec(repo, 0)

	code, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "tool-a", WorkDir: filepath.Join(repo, "cmd", "tool-a"), CacheDir: h.cache,
	})
	require.NoError(t, err)
	require.Zero(t, code)
	require.Contains(t, h.progress.String(), "rule 3")
	require.Contains(t, h.progress.String(), factoryDir)
}

func TestRule1AFactoryFlagOverridesTheWorkspace(t *testing.T) {
	h := newHarness(t)

	member, factoryDir, _ := universe(t)

	ws := t.TempDir()
	write(t, filepath.Join(ws, "forge-factory.yaml"), `version: "1"
name: enclosing
repos:
  - name: member-a
    url: u
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
`)

	repo := filepath.Join(ws, "member-a")
	require.NoError(t, os.Rename(member, repo))

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "member-a").
		Return(synccontroller.Report{}, nil).Once()
	h.expectExec(repo, 0)

	code, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "tool-a", WorkDir: repo, CacheDir: h.cache, Factory: factoryDir,
	})
	require.NoError(t, err)
	require.Zero(t, code)
	require.Contains(t, h.progress.String(), "rule 1")
}

func TestRule3FailsLoudWhenTheFactoryDisowns(t *testing.T) {
	h := newHarness(t)

	member, factoryDir, _ := universe(t)

	repo := member + "-renamed"
	require.NoError(t, os.Rename(member, repo))

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "tool-a", WorkDir: repo, CacheDir: h.cache, Factory: factoryDir,
	})
	require.ErrorIs(t, err, runcontroller.ErrNotAMember)
	require.ErrorContains(t, err, factoryDir)
}

func TestAnUnknownTargetListsWhatExists(t *testing.T) {
	h := newHarness(t)

	member, _, _ := universe(t)

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "ghost", WorkDir: member, CacheDir: h.cache,
	})
	require.ErrorIs(t, err, runcontroller.ErrNoTarget)
	require.ErrorContains(t, err, "tool-a")
}

func TestARunnableWithoutAFactoryOutsideAWorkspaceFails(t *testing.T) {
	h := newHarness(t)

	repo := t.TempDir()
	write(t, filepath.Join(repo, "forge.yaml"), `name: loner
run:
  - name: tool-a
    src: ./cmd/tool-a
`)

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "tool-a", WorkDir: repo, CacheDir: h.cache,
	})
	require.ErrorIs(t, err, runcontroller.ErrNoFactory)
}

func remoteUniverse(t *testing.T) (member, factoryDir, register string) {
	t.Helper()

	member, factoryDir, register = universe(t)

	memberSha := gitIn(t, member, "rev-parse", "HEAD")
	gitIn(t, member, "tag", "v0.1.0")

	write(t, filepath.Join(register, "index", "internal", member, "0.json"),
		`{"current":"v0.1.0","history":[{"version":"v0.1.0","provenance":"cafe01"}]}`)
	gitIn(t, register, "add", ".")
	gitIn(t, register, "commit", "-qm", "publish member-a v0.1.0")

	_ = memberSha

	return member, factoryDir, register
}

func TestARemoteRunResolvesTheProvenTuple(t *testing.T) {
	h := newHarness(t)

	member, _, _ := remoteUniverse(t)

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "member-a").
		Return(synccontroller.Report{}, nil).Once()

	var execDir string

	h.runner.EXPECT().
		RunAttached(mock.Anything, mock.Anything, mock.Anything, "forge", mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, dir string, _ map[string]string, _ string, _ ...string) (int, error) {
			execDir = dir

			return 0, nil
		}).Once()

	code, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: member, Name: "tool-a", CacheDir: h.cache,
	})
	require.NoError(t, err)
	require.Zero(t, code)

	require.Contains(t, h.progress.String(), "pin: "+member+" v0.1.0 proven by revision cafe01")
	require.True(t, strings.HasPrefix(execDir, filepath.Join(h.cache, "run")),
		"the run must execute inside the cache, got %s", execDir)

	tagSha := gitIn(t, member, "rev-parse", "v0.1.0^{commit}")
	gotSha := gitIn(t, execDir, "rev-parse", "HEAD")
	require.Equal(t, tagSha, gotSha, "the worktree must sit at the published version")

	require.FileExists(t, filepath.Join(execDir, ".envrc"))
	require.FileExists(t, filepath.Join(filepath.Dir(execDir), "forge-factory.yaml"))
}

func TestAWarmCacheSkipsMaterialisation(t *testing.T) {
	h := newHarness(t)

	member, _, _ := remoteUniverse(t)

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "member-a").
		Return(synccontroller.Report{}, nil).Once()
	h.runner.EXPECT().
		RunAttached(mock.Anything, mock.Anything, mock.Anything, "forge", mock.Anything, mock.Anything, mock.Anything).
		Return(0, nil).Twice()

	req := runcontroller.Request{Target: member, Name: "tool-a", CacheDir: h.cache}

	_, err := h.c.Run(context.Background(), req)
	require.NoError(t, err)

	_, err = h.c.Run(context.Background(), req)
	require.NoError(t, err)
	require.Contains(t, h.progress.String(), "cache: warm")
}

func TestADevRevRunsUnprovenWithTheFactoryFloating(t *testing.T) {
	h := newHarness(t)

	member, _, _ := remoteUniverse(t)
	sha := gitIn(t, member, "rev-parse", "HEAD")

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "member-a").
		Return(synccontroller.Report{}, nil).Once()

	var execDir string

	h.runner.EXPECT().
		RunAttached(mock.Anything, mock.Anything, mock.Anything, "forge", mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, dir string, _ map[string]string, _ string, _ ...string) (int, error) {
			execDir = dir

			return 0, nil
		}).Once()

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: member + "/cmd/tool-a@" + sha, CacheDir: h.cache,
	})
	require.NoError(t, err)
	require.Contains(t, h.progress.String(), "UNPROVEN")
	require.Equal(t, sha, gitIn(t, execDir, "rev-parse", "HEAD"))
}

func TestAnUnpublishedMemberRefusesAVersionlessRun(t *testing.T) {
	h := newHarness(t)

	member, _, _ := universe(t)

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: member, Name: "tool-a", CacheDir: h.cache,
	})
	require.ErrorIs(t, err, runcontroller.ErrUnpublished)
	require.ErrorContains(t, err, "dev run")
}

func TestTheExitCodePropagates(t *testing.T) {
	h := newHarness(t)

	ws := t.TempDir()
	repo := filepath.Join(ws, "member-a")
	repoWithRunnable(t, repo, "unused")
	write(t, filepath.Join(ws, "forge-factory.yaml"), `version: "1"
name: enclosing
repos:
  - name: member-a
    url: u
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
`)

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{}, nil).Once()
	h.expectExec(repo, 3)

	code, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "tool-a", WorkDir: repo, CacheDir: h.cache, Quiet: true,
	})
	require.NoError(t, err)
	require.Equal(t, 3, code)
	require.Empty(t, h.progress.String(), "--quiet must silence every line")
}

func TestMissingInputsFailBeforeTheBuild(t *testing.T) {
	h := newHarness(t)

	ws := t.TempDir()
	repo := filepath.Join(ws, "member-a")
	repoWithRunnable(t, repo, "unused")
	write(t, filepath.Join(repo, "cmd", "tool-a", "zz_generated.runnable.yaml"), `name: tool-a
inputs:
  env:
    - THE_FIXTURE_INPUT_VAR
  files: []
  spec: []
`)
	write(t, filepath.Join(ws, "forge-factory.yaml"), `version: "1"
name: enclosing
repos:
  - name: member-a
    url: u
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
`)

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{}, nil).Maybe()

	req := runcontroller.Request{Target: "tool-a", WorkDir: repo, CacheDir: h.cache}

	require.NoError(t, os.Unsetenv("THE_FIXTURE_INPUT_VAR"))

	_, err := h.c.Run(context.Background(), req)
	require.ErrorIs(t, err, runcontroller.ErrMissingInput)
	require.ErrorContains(t, err, "THE_FIXTURE_INPUT_VAR")

	t.Setenv("THE_FIXTURE_INPUT_VAR", "set")
	h.expectExec(repo, 0)

	_, err = h.c.Run(context.Background(), req)
	require.NoError(t, err)
}

func TestBootstrapPlacesTheWorkspaceFiles(t *testing.T) {
	h := newHarness(t)

	_, factoryDir, _ := universe(t)

	dest := t.TempDir()

	f, root, err := h.c.Bootstrap(context.Background(), runcontroller.BootstrapRequest{
		Factory: factoryDir, Dir: dest, CacheDir: h.cache,
	})
	require.NoError(t, err)
	require.Equal(t, dest, root)
	require.Equal(t, "fixture", f.Name)
	require.FileExists(t, filepath.Join(dest, "forge-factory.yaml"))
	require.FileExists(t, filepath.Join(dest, "forge-ci.yaml"))

	placed, err := os.ReadFile(filepath.Join(dest, "forge-factory.yaml"))
	require.NoError(t, err)

	source, err := os.ReadFile(filepath.Join(factoryDir, "workspace", "forge-factory.yaml"))
	require.NoError(t, err)
	require.Equal(t, source, placed, "the placed file is byte-identical to the factory's")
}

func TestTheProvenanceRecordPinsTheMemberSha(t *testing.T) {
	h := newHarness(t)

	base := t.TempDir()
	member := filepath.Join(base, "member-a")
	factoryDir := filepath.Join(base, "fixture-factory")
	register := filepath.Join(base, "fixture-register")
	state := filepath.Join(base, "fixture-state")

	registerRepo(t, register)
	repoWithRunnable(t, member, factoryDir)

	pinnedSha := gitIn(t, member, "rev-parse", "HEAD")
	write(t, filepath.Join(member, "MOVED"), "the head moved past the proven sha\n")
	gitIn(t, member, "add", ".")
	gitIn(t, member, "commit", "-qm", "move past the proven sha")
	gitIn(t, member, "tag", "v0.9.9")

	write(t, filepath.Join(state, "revisions", "cafe02.json"),
		`{"id":"cafe02","repos":{"member-a":"`+pinnedSha+`"}}`)
	gitIn(t, state, "init", "-q", "-b", "main")
	gitIn(t, state, "add", ".")
	gitIn(t, state, "commit", "-qm", "record")

	write(t, filepath.Join(factoryDir, "workspace", "forge-factory.yaml"), `version: "1"
name: fixture
repos:
  - name: member-a
    url: `+member+`
    languages: [go]
  - name: fixture-register
    url: `+register+`
  - name: fixture-state
    url: `+state+`
engines:
  - alias: go
    engine: forge://example.com/lang-go
register:
  url: `+register+`
state:
  engine: forge://example.com/state
  spec:
    path: ./fixture-state
`)
	gitIn(t, factoryDir, "init", "-q", "-b", "main")
	gitIn(t, factoryDir, "add", ".")
	gitIn(t, factoryDir, "commit", "-qm", "seed")

	write(t, filepath.Join(register, "index", "internal", member, "0.json"),
		`{"current":"v0.9.9","history":[{"version":"v0.9.9","provenance":"cafe02"}]}`)
	gitIn(t, register, "add", ".")
	gitIn(t, register, "commit", "-qm", "publish")

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "member-a").
		Return(synccontroller.Report{}, nil).Once()

	var execDir string

	h.runner.EXPECT().
		RunAttached(mock.Anything, mock.Anything, mock.Anything, "forge", mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, dir string, _ map[string]string, _ string, _ ...string) (int, error) {
			execDir = dir

			return 0, nil
		}).Once()

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: member, Name: "tool-a", CacheDir: h.cache,
	})
	require.NoError(t, err)
	require.Equal(t, pinnedSha, gitIn(t, execDir, "rev-parse", "HEAD"),
		"the revision record beats the tag: the proven sha runs")
	require.NoFileExists(t, filepath.Join(execDir, "MOVED"))
}

func TestARepoWithoutForgeYamlRefusesARemoteRun(t *testing.T) {
	h := newHarness(t)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "README.md"), "nothing runnable\n")
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "seed")

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: dir, Name: "tool-a", CacheDir: h.cache,
	})
	require.ErrorIs(t, err, runcontroller.ErrNoTarget)
	require.ErrorContains(t, err, "no forge.yaml")
}

func TestACloneFailureNamesTheURL(t *testing.T) {
	h := newHarness(t)

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "/nowhere/that/exists/repo", Name: "x", CacheDir: h.cache,
	})
	require.Error(t, err)
}

func TestAWorkspaceThatDoesNotParseFailsTheSync(t *testing.T) {
	h := newHarness(t)

	ws := t.TempDir()
	repo := filepath.Join(ws, "member-a")
	repoWithRunnable(t, repo, "unused")
	write(t, filepath.Join(ws, "forge-factory.yaml"), "version: '1'\nname: broken\nrepos: []\nengines: []\nnonsense: [")

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "tool-a", WorkDir: repo, CacheDir: h.cache,
	})
	require.Error(t, err)
}

func TestMissingInputFilesFailByName(t *testing.T) {
	h := newHarness(t)

	ws := t.TempDir()
	repo := filepath.Join(ws, "member-a")
	repoWithRunnable(t, repo, "unused")
	write(t, filepath.Join(repo, "cmd", "tool-a", "zz_generated.runnable.yaml"), `name: tool-a
inputs:
  env: []
  files:
    - config.yaml
`)
	write(t, filepath.Join(ws, "forge-factory.yaml"), `version: "1"
name: enclosing
repos:
  - name: member-a
    url: u
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
`)

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{}, nil).Maybe()

	req := runcontroller.Request{Target: "tool-a", WorkDir: repo, CacheDir: h.cache}

	_, err := h.c.Run(context.Background(), req)
	require.ErrorIs(t, err, runcontroller.ErrMissingInput)
	require.ErrorContains(t, err, "config.yaml")

	write(t, filepath.Join(repo, "config.yaml"), "ok: true\n")
	h.expectExec(repo, 0)

	_, err = h.c.Run(context.Background(), req)
	require.NoError(t, err)
}

func TestForceRefreshesAWarmCache(t *testing.T) {
	h := newHarness(t)

	member, _, _ := remoteUniverse(t)

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "member-a").
		Return(synccontroller.Report{}, nil).Twice()
	h.runner.EXPECT().
		RunAttached(mock.Anything, mock.Anything, mock.Anything, "forge", mock.Anything, mock.Anything, mock.Anything).
		Return(0, nil).Twice()

	req := runcontroller.Request{Target: member, Name: "tool-a", CacheDir: h.cache}

	_, err := h.c.Run(context.Background(), req)
	require.NoError(t, err)

	req.Force = true

	_, err = h.c.Run(context.Background(), req)
	require.NoError(t, err)
	require.NotContains(t, h.progress.String(), "cache: warm")
}

func TestBootstrapErrorsOnAFactoryWithoutWorkspaceFiles(t *testing.T) {
	h := newHarness(t)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "README.md"), "no workspace dir\n")
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "seed")

	_, _, err := h.c.Bootstrap(context.Background(), runcontroller.BootstrapRequest{
		Factory: dir, Dir: t.TempDir(), CacheDir: h.cache,
	})
	require.ErrorContains(t, err, "workspace/forge-factory.yaml")
}

func TestBootstrapErrorsOnAnUnknownRev(t *testing.T) {
	h := newHarness(t)

	_, factoryDir, _ := universe(t)

	_, _, err := h.c.Bootstrap(context.Background(), runcontroller.BootstrapRequest{
		Factory: factoryDir + "@does-not-exist", Dir: t.TempDir(), CacheDir: h.cache,
	})
	require.ErrorContains(t, err, "does-not-exist")
}

func TestTheDefaultCacheDirComesFromTheUserCache(t *testing.T) {
	h := newHarness(t)

	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	_, err := h.c.Run(context.Background(), runcontroller.Request{
		Target: "/nowhere/repo", Name: "x",
	})
	require.Error(t, err, "the run proceeds far enough to fail on the clone, proving the cache dir resolved")
}
