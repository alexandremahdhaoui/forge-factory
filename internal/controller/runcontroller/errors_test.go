package runcontroller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/lockadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/gitadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/synccontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

const shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

const toolForgeYaml = `name: tool
run:
  - name: tool
    src: ./cmd/tool
    factory: git@example.com:org/factory.git
`

const orphanForgeYaml = `name: tool
run:
  - name: tool
    src: ./cmd/tool
`

const claimingFactoryYaml = `version: v1
name: fixture
repos:
  - name: tool
    url: git@example.com:org/tool.git
engines:
  - alias: gg
    engine: forge://generic-builder
`

const registeredFactoryYaml = claimingFactoryYaml + `register:
  url: git@example.com:org/register.git
`

type fakeFS struct {
	files     map[string]string
	dirs      map[string]bool
	readErr   map[string]error
	writeErr  map[string]error
	existsErr map[string]error
	renameErr map[string]error
	writes    map[string]string
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:     map[string]string{},
		dirs:      map[string]bool{},
		readErr:   map[string]error{},
		writeErr:  map[string]error{},
		existsErr: map[string]error{},
		renameErr: map[string]error{},
		writes:    map[string]string{},
	}
}

func (f *fakeFS) ReadFile(path string) ([]byte, error) {
	if err := f.readErr[path]; err != nil {
		return nil, err
	}

	if content, ok := f.files[path]; ok {
		return []byte(content), nil
	}

	if content, ok := f.writes[path]; ok {
		return []byte(content), nil
	}

	return nil, fmt.Errorf("reading %s: %w", path, os.ErrNotExist)
}

func (f *fakeFS) WriteFile(path string, data []byte) error {
	if err := f.writeErr[path]; err != nil {
		return err
	}

	f.writes[path] = string(data)

	return nil
}

func (f *fakeFS) MkdirAll(path string) error { return nil }

func (f *fakeFS) Exists(path string) (bool, error) {
	if err := f.existsErr[path]; err != nil {
		return false, err
	}

	if _, ok := f.files[path]; ok {
		return true, nil
	}

	if _, ok := f.writes[path]; ok {
		return true, nil
	}

	return f.dirs[path], nil
}

func (f *fakeFS) IsDir(path string) (bool, error) { return f.dirs[path], nil }

func (f *fakeFS) List(dir string) ([]string, error) { return nil, nil }

func (f *fakeFS) Remove(path string) error { return nil }

func (f *fakeFS) WriteExecutable(path string, data []byte) error { return f.WriteFile(path, data) }

func (f *fakeFS) Rename(oldPath, newPath string) error {
	if err := f.renameErr[oldPath]; err != nil {
		return err
	}

	f.writes[newPath] = f.writes[oldPath]
	delete(f.writes, oldPath)

	return nil
}

func (f *fakeFS) Symlink(target, link string) error {
	f.writes[link] = "-> " + target

	return nil
}

type rig struct {
	fs   *fakeFS
	git  *gitadaptermock.MockGit
	exec *execadaptermock.MockRunner
	sync *synccontrollermock.MockSyncer
	out  *bytes.Buffer
	c    *Controller
}

func newRig(t *testing.T) *rig {
	t.Helper()

	r := &rig{
		fs:   newFakeFS(),
		git:  gitadaptermock.NewMockGit(t),
		exec: execadaptermock.NewMockRunner(t),
		sync: synccontrollermock.NewMockSyncer(t),
		out:  &bytes.Buffer{},
	}
	r.c = New(r.fs, r.git, r.exec, r.sync, nopLocker{}, r.out)

	// The run context locks right after syncing; these tests are about
	// other failure shapes, so the lock always resolves.
	r.sync.EXPECT().Lock(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{}, nil).Maybe()

	// The exec boundary asks PATH for forge before picking a pinned go-run
	// fallback; answering yes keeps the bare form every expectation pins.
	r.exec.EXPECT().LookPath("forge").Return("/usr/bin/forge", true).Maybe()

	return r
}

func TestRunFailsWhenNoCacheBaseExists(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	r := newRig(t)

	_, err := r.c.Run(context.Background(), Request{Target: "tool"})
	require.ErrorContains(t, err, "finding the cache directory")
}

func TestBootstrapFailsWhenNoCacheBaseExists(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	r := newRig(t)

	_, _, err := r.c.Bootstrap(context.Background(), BootstrapRequest{Factory: "git@example.com:org/factory.git"})
	require.ErrorContains(t, err, "finding the cache directory")
}

func TestRunLocalFailsWithNoRepoInReach(t *testing.T) {
	r := newRig(t)

	_, err := r.c.Run(context.Background(), Request{Target: "tool", WorkDir: t.TempDir(), CacheDir: t.TempDir()})
	require.ErrorContains(t, err, "finding the repo")
}

func TestRunLocalFailsWhenForgeYamlIsUnreadable(t *testing.T) {
	r := newRig(t)
	r.fs.files["/repo/forge.yaml"] = ""
	r.fs.readErr["/repo/forge.yaml"] = errors.New("disk gone")

	_, err := r.c.Run(context.Background(), Request{Target: "tool", WorkDir: "/repo", CacheDir: t.TempDir()})
	require.ErrorContains(t, err, "disk gone")
}

func TestRunLocalFailsOnMalformedForgeYaml(t *testing.T) {
	r := newRig(t)
	r.fs.files["/repo/forge.yaml"] = "{["

	_, err := r.c.Run(context.Background(), Request{Target: "tool", WorkDir: "/repo", CacheDir: t.TempDir()})
	require.ErrorContains(t, err, "reading forge.yaml")
}

func TestRuleTwoFailsWhenTheWorkspaceFactoryVanishes(t *testing.T) {
	r := newRig(t)
	r.fs.files["/ws/tool/forge.yaml"] = toolForgeYaml
	r.fs.files["/ws/forge-factory.yaml"] = claimingFactoryYaml

	calls := 0
	r.fs.readErr["/ws/forge-factory.yaml"] = nil

	original := r.fs.files["/ws/forge-factory.yaml"]
	r.fs.files["/ws/forge-factory.yaml"] = original

	readGate := func(path string) ([]byte, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("factory vanished")
		}

		return []byte(original), nil
	}
	r.fs.readErr["/ws/forge-factory.yaml"] = nil
	gated := &gatedFS{fakeFS: r.fs, gate: map[string]func(string) ([]byte, error){"/ws/forge-factory.yaml": readGate}}
	r.c = New(gated, r.git, r.exec, r.sync, lockadapter.New(), r.out)

	_, err := r.c.Run(context.Background(), Request{Target: "tool", WorkDir: "/ws/tool", CacheDir: t.TempDir()})
	require.ErrorContains(t, err, "factory vanished")
}

type gatedFS struct {
	*fakeFS
	gate map[string]func(string) ([]byte, error)
}

func (g *gatedFS) ReadFile(path string) ([]byte, error) {
	if fn, ok := g.gate[path]; ok {
		return fn(path)
	}

	return g.fakeFS.ReadFile(path)
}

func TestRuleTwoFailsWhenTheSyncFails(t *testing.T) {
	r := newRig(t)
	r.fs.files["/ws/tool/forge.yaml"] = toolForgeYaml
	r.fs.files["/ws/forge-factory.yaml"] = claimingFactoryYaml

	r.sync.EXPECT().Sync(mock.Anything, mock.Anything, "/ws", "").
		Return(synccontroller.Report{}, errors.New("manifest write refused"))

	_, err := r.c.Run(context.Background(), Request{Target: "tool", WorkDir: "/ws/tool", CacheDir: t.TempDir()})
	require.ErrorContains(t, err, "manifest write refused")
	require.ErrorContains(t, err, "syncing /ws")
}

func remoteReq(t *testing.T, target string) Request {
	t.Helper()

	return Request{Target: target, Name: "tool", Quiet: true, CacheDir: t.TempDir()}
}

func TestRemoteFailsWhenTheDefaultBranchDoesNotResolve(t *testing.T) {
	r := newRig(t)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/tool.git", mock.Anything).Return(nil)
	r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").
		Return("", errors.New("no HEAD"))

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "resolving the default branch of example.com/org/tool")
}

func TestRemoteFailsWhenForgeYamlCannotBeShown(t *testing.T) {
	r := newRig(t)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/tool.git", mock.Anything).Return(nil)
	r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "forge.yaml").
		Return("", false, errors.New("object store corrupt"))

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "object store corrupt")
}

func TestRemoteFailsWhenTheRunnableDeclaresNoFactory(t *testing.T) {
	r := newRig(t)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/tool.git", mock.Anything).Return(nil)
	r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "forge.yaml").
		Return(orphanForgeYaml, true, nil)

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorIs(t, err, ErrNoFactory)
}

func TestRemoteFactoryFlagOverridesTheRunnables(t *testing.T) {
	r := newRig(t)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/tool.git", mock.Anything).Return(nil)
	r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil).Once()
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "forge.yaml").
		Return(toolForgeYaml, true, nil)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/other.git", mock.Anything).
		Return(errors.New("no such factory"))

	req := remoteReq(t, "example.com/org/tool")
	req.Factory = "git@example.com:org/other.git@v2"
	req.Quiet = false

	_, err := r.c.Run(context.Background(), req)
	require.ErrorContains(t, err, "no such factory")
	require.Contains(t, r.out.String(), "--factory git@example.com:org/other.git overrides the runnable's")
}

func TestRemoteDevRevMustResolve(t *testing.T) {
	r := newRig(t)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/tool.git", mock.Anything).Return(nil)
	r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "forge.yaml").
		Return(toolForgeYaml, true, nil)
	r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "v9.9.9").
		Return("", errors.New("unknown revision"))

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool/cmd/tool@v9.9.9"))
	require.ErrorContains(t, err, "resolving v9.9.9 in example.com/org/tool")
}

// remoteToFactory drives a remote run up to the factory read and hands the
// test the factory content to answer with.
func (r *rig) remoteToFactory(factoryYaml string) {
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/tool.git", mock.Anything).Return(nil)
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "forge.yaml").
		Return(toolForgeYaml, true, nil)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/factory.git", mock.Anything).Return(nil)
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "workspace/forge-factory.yaml").
		Return(factoryYaml, true, nil)
	r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
}

func TestRemoteRefusesANonMember(t *testing.T) {
	r := newRig(t)

	stranger := strings.ReplaceAll(registeredFactoryYaml, "name: tool", "name: other")
	stranger = strings.ReplaceAll(stranger, "org/tool.git", "org/other.git")
	r.remoteToFactory(stranger)

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorIs(t, err, ErrNotAMember)
	require.ErrorContains(t, err, "git@example.com:org/factory.git")
}

func TestRemoteRefusesAFactoryWithoutARegister(t *testing.T) {
	r := newRig(t)
	r.remoteToFactory(claimingFactoryYaml)

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "names no register")
}

func TestRemoteFailsWhenTheRegisterCloneFails(t *testing.T) {
	r := newRig(t)
	r.remoteToFactory(registeredFactoryYaml)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/register.git", mock.Anything).
		Return(errors.New("register unreachable"))

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "register unreachable")
}

func TestRemoteFailsWhenTheRegisterRevisionDoesNotResolve(t *testing.T) {
	r := newRig(t)

	pinned := registeredFactoryYaml + "  revision: v9\n"
	r.remoteToFactory(pinned)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/register.git", mock.Anything).Return(nil)
	r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "v9").
		Return("", errors.New("unknown revision"))

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "resolving the register at v9")
}

// remoteToRegister continues past the factory to a resolved register clone.
func (r *rig) remoteToRegister() {
	r.remoteToFactory(registeredFactoryYaml)
	r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/register.git", mock.Anything).Return(nil)
}

func TestRemoteFailsWhenTheTrackListingFails(t *testing.T) {
	r := newRig(t)
	r.remoteToRegister()
	r.git.EXPECT().LsTree(mock.Anything, mock.Anything, shaA, "index/internal/example.com/org/tool").
		Return(nil, errors.New("tree walk failed"))

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "tree walk failed")
}

func TestRemoteFailsWhenTheTrackFileCannotBeRead(t *testing.T) {
	r := newRig(t)
	r.remoteToRegister()
	r.git.EXPECT().LsTree(mock.Anything, mock.Anything, shaA, "index/internal/example.com/org/tool").
		Return([]string{"README", "1.json"}, nil)
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "index/internal/example.com/org/tool/1.json").
		Return("", false, errors.New("blob missing"))

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "reading the internal track of example.com/org/tool")
}

func TestRemoteFailsOnAMalformedTrackFile(t *testing.T) {
	r := newRig(t)
	r.remoteToRegister()
	r.git.EXPECT().LsTree(mock.Anything, mock.Anything, shaA, "index/internal/example.com/org/tool").
		Return([]string{"1.json"}, nil)
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "index/internal/example.com/org/tool/1.json").
		Return("{[", true, nil)

	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "decoding the internal track of example.com/org/tool")
}

func TestRemoteFailsWhenNothingPinsTheVersion(t *testing.T) {
	r := newRig(t)
	r.remoteToRegister()
	r.git.EXPECT().LsTree(mock.Anything, mock.Anything, shaA, "index/internal/example.com/org/tool").
		Return([]string{"1.json"}, nil)
	r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "index/internal/example.com/org/tool/1.json").
		Return(`{"current":"v1.0.0"}`, true, nil)

	// A track that names a version and no proving revision is not a run that
	// can be pinned. Saying so here beats letting it fall through to "the
	// revision names no sha", which names the symptom and sends the reader
	// looking in the state repo instead of at the track.
	_, err := r.c.Run(context.Background(), remoteReq(t, "example.com/org/tool"))
	require.ErrorContains(t, err, "names v1.0.0 with no proving revision")
}

func TestProvenancePinShapes(t *testing.T) {
	statefulFactory := func(statePath string) config.Factory {
		f, err := config.Parse([]byte(registeredFactoryYaml + `  path: ./register
state:
  engine: forge://ci-state-git
  spec:
    path: ` + statePath + "\n"))
		require.NoError(t, err)

		return f
	}

	ctx := context.Background()

	t.Run("no provenance answers nothing", func(t *testing.T) {
		r := newRig(t)
		require.Nil(t, r.c.provenancePins(ctx, Request{}, config.Factory{}, ""))
	})

	t.Run("a state entry without a path answers nothing", func(t *testing.T) {
		r := newRig(t)
		f := statefulFactory("./state")
		f.State.Spec = map[string]any{}
		require.Nil(t, r.c.provenancePins(ctx, Request{Quiet: true}, f, "cafe"))
	})

	t.Run("a state repo outside the members answers nothing", func(t *testing.T) {
		r := newRig(t)
		require.Nil(t, r.c.provenancePins(ctx, Request{Quiet: true}, statefulFactory("./state"), "cafe"))
	})

	stateMember := func(t *testing.T) config.Factory {
		t.Helper()

		f := statefulFactory("./state")
		f.Repos = append(f.Repos, config.Repo{Name: "state", URL: "git@example.com:org/state.git"})

		return f
	}

	t.Run("a failing clone answers nothing", func(t *testing.T) {
		r := newRig(t)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/state.git", mock.Anything).
			Return(errors.New("down"))
		require.Nil(t, r.c.provenancePins(ctx, Request{Quiet: true, CacheDir: t.TempDir()}, stateMember(t), "cafe"))
	})

	t.Run("an unresolvable head answers nothing", func(t *testing.T) {
		r := newRig(t)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/state.git", mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").
			Return("", errors.New("no HEAD"))
		require.Nil(t, r.c.provenancePins(ctx, Request{Quiet: true, CacheDir: t.TempDir()}, stateMember(t), "cafe"))
	})

	t.Run("a missing revision record floats with one line", func(t *testing.T) {
		r := newRig(t)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/state.git", mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "revisions/cafe.json").
			Return("", false, nil)
		require.Nil(t, r.c.provenancePins(ctx, Request{CacheDir: t.TempDir()}, stateMember(t), "cafe"))
		require.Contains(t, r.out.String(), "revision cafe is not in state")
	})

	t.Run("a malformed record answers nothing", func(t *testing.T) {
		r := newRig(t)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/state.git", mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "revisions/cafe.json").
			Return("{[", true, nil)
		require.Nil(t, r.c.provenancePins(ctx, Request{Quiet: true, CacheDir: t.TempDir()}, stateMember(t), "cafe"))
	})

	t.Run("a record answers its repo shas", func(t *testing.T) {
		r := newRig(t)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/state.git", mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "revisions/cafe.json").
			Return(fmt.Sprintf(`{"repos":{"tool":%q}}`, shaB), true, nil)

		pins := r.c.provenancePins(ctx, Request{Quiet: true, CacheDir: t.TempDir()}, stateMember(t), "cafe")
		require.Equal(t, map[string]string{"tool": shaB}, pins)
	})
}

func TestFactoryAtErrorShapes(t *testing.T) {
	ctx := context.Background()

	t.Run("a failing show fails", func(t *testing.T) {
		r := newRig(t)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/factory.git", mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "workspace/forge-factory.yaml").
			Return("", false, errors.New("blob gone"))

		_, _, err := r.c.factoryAt(ctx, t.TempDir(), "git@example.com:org/factory.git", "")
		require.ErrorContains(t, err, "blob gone")
	})

	t.Run("a malformed factory fails", func(t *testing.T) {
		r := newRig(t)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/factory.git", mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "workspace/forge-factory.yaml").
			Return("{[", true, nil)

		_, _, err := r.c.factoryAt(ctx, t.TempDir(), "git@example.com:org/factory.git", "")
		require.ErrorContains(t, err, "reading the factory at git@example.com:org/factory.git")
	})
}

func TestCloneOrFetchShapes(t *testing.T) {
	ctx := context.Background()

	t.Run("a warm clone fetches and a failing fetch fails", func(t *testing.T) {
		r := newRig(t)
		cache := t.TempDir()
		r.fs.dirs[filepath.Join(cache, "git", sanitize("git@example.com:org/tool.git"))] = true
		r.git.EXPECT().Fetch(mock.Anything, mock.Anything).Return(errors.New("offline"))

		_, err := r.c.cloneOrFetch(ctx, cache, "git@example.com:org/tool.git")
		require.ErrorContains(t, err, "offline")
	})

	t.Run("an uncreatable cache fails", func(t *testing.T) {
		r := newRig(t)
		r.withRealLock()
		cache := filepath.Join(t.TempDir(), "blocked")
		require.NoError(t, os.WriteFile(cache, []byte(""), 0o600))

		// The lock is the first thing to touch the blocked path now, and
		// its failure names the same uncreatable directory.
		_, err := r.c.cloneOrFetch(ctx, cache, "git@example.com:org/tool.git")
		require.ErrorContains(t, err, "preparing the lock")
	})
}

func TestCheckoutContextErrorShapes(t *testing.T) {
	ctx := context.Background()
	factory := materialFactory{raw: []byte(registeredFactoryYaml)}
	member := config.Repo{Name: "tool", URL: "git@example.com:org/tool.git"}
	registerURL := "git@example.com:org/register.git"

	// Directories, not decoration: a real adapter may reach these, and a
	// literal "/clone" is only writable for root.
	shared := t.TempDir()
	clone := filepath.Join(shared, "clone")
	register := filepath.Join(shared, "register")

	t.Run("an uncreatable root fails", func(t *testing.T) {
		r := newRig(t)
		blocked := filepath.Join(t.TempDir(), "blocked")
		require.NoError(t, os.WriteFile(blocked, []byte(""), 0o600))

		err := r.c.checkoutContext(ctx, Request{Quiet: true}, filepath.Join(blocked, "root"),
			factory, member, clone, shaA, register, shaB, registerURL)
		require.ErrorContains(t, err, "creating the run context")
	})

	t.Run("an unplaceable factory file fails", func(t *testing.T) {
		r := newRig(t)
		root := t.TempDir()
		r.fs.writeErr[filepath.Join(root, "forge-factory.yaml")] = errors.New("read only")

		err := r.c.checkoutContext(ctx, Request{Quiet: true}, root,
			factory, member, clone, shaA, register, shaB, registerURL)
		require.ErrorContains(t, err, "placing the factory file")
	})

	t.Run("a failing repo worktree fails", func(t *testing.T) {
		r := newRig(t)
		root := t.TempDir()
		r.git.EXPECT().WorktreeAdd(mock.Anything, clone, shaA, filepath.Join(root, "tool")).
			Return(errors.New("worktree refused"))

		err := r.c.checkoutContext(ctx, Request{Quiet: true}, root,
			factory, member, clone, shaA, register, shaB, registerURL)
		require.ErrorContains(t, err, "worktree refused")
	})

	t.Run("a failing register worktree fails", func(t *testing.T) {
		r := newRig(t)
		root := t.TempDir()
		r.fs.dirs[filepath.Join(root, "tool")] = true
		r.git.EXPECT().WorktreeAdd(mock.Anything, register, shaB, filepath.Join(root, "register")).
			Return(errors.New("register worktree refused"))

		err := r.c.checkoutContext(ctx, Request{Quiet: true}, root,
			factory, member, clone, shaA, register, shaB, registerURL)
		require.ErrorContains(t, err, "register worktree refused")
	})

	t.Run("an unwritable envrc fails", func(t *testing.T) {
		r := newRig(t)
		root := t.TempDir()
		r.fs.dirs[filepath.Join(root, "tool")] = true
		r.fs.dirs[filepath.Join(root, "register")] = true
		r.fs.writeErr[filepath.Join(root, "tool", ".envrc")] = errors.New("read only")

		err := r.c.checkoutContext(ctx, Request{Quiet: true}, root,
			factory, member, clone, shaA, register, shaB, registerURL)
		require.ErrorContains(t, err, "creating "+filepath.Join(root, "tool", ".envrc"))
	})
}

func TestMaterialiseAndExecErrorShapes(t *testing.T) {
	ctx := context.Background()
	target := runnable{Name: "tool", Src: "./cmd/tool"}
	factoryURL := "git@example.com:org/factory.git"

	factoryFlow := func(r *rig, yaml string) {
		r.git.EXPECT().Clone(mock.Anything, factoryURL, mock.Anything).Return(nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "workspace/forge-factory.yaml").
			Return(yaml, true, nil)
	}

	t.Run("an uncreatable context root fails", func(t *testing.T) {
		r := newRig(t)
		r.withRealLock()
		factoryFlow(r, claimingFactoryYaml)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)

		cache := filepath.Join(t.TempDir(), "cache")
		require.NoError(t, os.MkdirAll(filepath.Join(cache, "git"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(cache, "run"), []byte(""), 0o600))

		// The lock is the first thing to touch the blocked path now, and
		// its failure names the same uncreatable directory.
		_, err := r.c.materialiseAndExec(ctx, Request{Quiet: true, CacheDir: cache}, "/ws/tool", target, factoryURL, "", "")
		require.ErrorContains(t, err, "preparing the lock")
	})

	t.Run("an unplaceable factory file fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r, claimingFactoryYaml)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)

		cache := t.TempDir()
		root := filepath.Join(cache, "run", "local--ws-tool+"+sanitize(factoryURL)+"@"+shortSha(shaA))
		r.fs.writeErr[filepath.Join(root, "forge-factory.yaml")] = errors.New("read only")

		_, err := r.c.materialiseAndExec(ctx, Request{Quiet: true, CacheDir: cache}, "/ws/tool", target, factoryURL, "", "")
		require.ErrorContains(t, err, "placing the factory file")
	})

	t.Run("a blocked symlink fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r, claimingFactoryYaml)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)

		cache := t.TempDir()
		root := filepath.Join(cache, "run", "local--ws-tool+"+sanitize(factoryURL)+"@"+shortSha(shaA))
		require.NoError(t, os.MkdirAll(root, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, "tool"), []byte(""), 0o600))

		_, err := r.c.materialiseAndExec(ctx, Request{Quiet: true, CacheDir: cache}, "/ws/tool", target, factoryURL, "", "")
		require.ErrorContains(t, err, "linking the checkout into the context")
	})

	t.Run("a failing register clone fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r, registeredFactoryYaml)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/register.git", mock.Anything).
			Return(errors.New("register unreachable"))

		_, err := r.c.materialiseAndExec(ctx, Request{Quiet: true, CacheDir: t.TempDir()}, "/ws/tool", target, factoryURL, "", "")
		require.ErrorContains(t, err, "register unreachable")
	})

	t.Run("an unresolvable pinned register fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r, registeredFactoryYaml+"  revision: v3\n")
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/register.git", mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "v3").
			Return("", errors.New("unknown revision"))

		_, err := r.c.materialiseAndExec(ctx, Request{Quiet: true, CacheDir: t.TempDir()}, "/ws/tool", target, factoryURL, "", "")
		require.ErrorContains(t, err, "resolving the register at v3")
	})

	t.Run("a failing register worktree fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r, registeredFactoryYaml)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil).Times(2)
		r.git.EXPECT().Clone(mock.Anything, "git@example.com:org/register.git", mock.Anything).Return(nil)
		r.git.EXPECT().WorktreeAdd(mock.Anything, mock.Anything, shaA, mock.Anything).
			Return(errors.New("worktree refused"))

		_, err := r.c.materialiseAndExec(ctx, Request{Quiet: true, CacheDir: t.TempDir()}, "/ws/tool", target, factoryURL, "", "")
		require.ErrorContains(t, err, "worktree refused")
	})

	t.Run("a failing sync fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r, claimingFactoryYaml)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "tool").
			Return(synccontroller.Report{}, errors.New("manifest write refused"))

		cache := t.TempDir()
		root := filepath.Join(cache, "run", "local--ws-tool+"+sanitize(factoryURL)+"@"+shortSha(shaA))
		r.fs.writes[filepath.Join(root, "forge-factory.yaml")] = claimingFactoryYaml
		r.fs.files[filepath.Join(root, "forge-factory.yaml")] = claimingFactoryYaml
		r.fs.dirs[filepath.Join(root, "tool")] = true

		_, err := r.c.materialiseAndExec(ctx, Request{Quiet: true, CacheDir: cache}, "/ws/tool", target, factoryURL, "", "")
		require.ErrorContains(t, err, "manifest write refused")
	})

	t.Run("a malformed runnable manifest fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r, claimingFactoryYaml)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, "tool").
			Return(synccontroller.Report{}, nil)

		cache := t.TempDir()
		root := filepath.Join(cache, "run", "local--ws-tool+"+sanitize(factoryURL)+"@"+shortSha(shaA))
		r.fs.files[filepath.Join(root, "forge-factory.yaml")] = claimingFactoryYaml
		r.fs.dirs[filepath.Join(root, "tool")] = true
		r.fs.files["/ws/tool/cmd/tool/zz_generated.runnable.yaml"] = "{["

		_, err := r.c.materialiseAndExec(ctx, Request{Quiet: true, CacheDir: cache}, "/ws/tool", target, factoryURL, "", "")
		require.ErrorContains(t, err, "reading /ws/tool/cmd/tool/zz_generated.runnable.yaml")
	})
}

func TestBootstrapErrorShapes(t *testing.T) {
	ctx := context.Background()
	factoryURL := "git@example.com:org/factory.git"

	factoryFlow := func(r *rig) {
		r.git.EXPECT().Clone(mock.Anything, factoryURL, mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "workspace/forge-factory.yaml").
			Return(claimingFactoryYaml, true, nil)
	}

	t.Run("an uncreatable directory fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)

		blocked := filepath.Join(t.TempDir(), "blocked")
		require.NoError(t, os.WriteFile(blocked, []byte(""), 0o600))

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: filepath.Join(blocked, "ws"), CacheDir: t.TempDir(), Quiet: true,
		})
		require.ErrorContains(t, err, "creating")
	})

	t.Run("an unplaceable factory file fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)

		dir := t.TempDir()
		r.fs.writeErr[filepath.Join(dir, "forge-factory.yaml")] = errors.New("read only")

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.ErrorContains(t, err, "placing forge-factory.yaml")
	})

	t.Run("an unplaceable extra fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "workspace/forge-ci.yaml").
			Return("pipelines: []", true, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, mock.Anything).
			Return("", false, nil).Maybe()

		dir := t.TempDir()
		r.fs.writeErr[filepath.Join(dir, "forge-ci.yaml")] = errors.New("read only")

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.ErrorContains(t, err, "placing forge-ci.yaml")
	})
}

func TestBootstrapProtectsThePlacedWorkspaceFiles(t *testing.T) {
	ctx := context.Background()
	factoryURL := "git@example.com:org/factory.git"

	const oldCommittedYaml = "version: v1\nname: fixture\nrepos: []\n"

	const handEditedYaml = "version: v1\nname: fixture\n# a person changed this by hand\nrepos: []\n"

	factoryFlow := func(r *rig) {
		r.git.EXPECT().Clone(mock.Anything, factoryURL, mock.Anything).Return(nil)
		r.git.EXPECT().ResolveRev(mock.Anything, mock.Anything, "origin/HEAD").Return(shaA, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, "workspace/forge-factory.yaml").
			Return(claimingFactoryYaml, true, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaA, mock.Anything).
			Return("", false, nil).Maybe()
	}

	t.Run("an unchanged file is rewritten silently", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = claimingFactoryYaml

		_, root, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.NoError(t, err)
		require.Equal(t, dir, root)
		require.Equal(t, claimingFactoryYaml, r.fs.writes[dest])
	})

	t.Run("a file matching an older committed version is rewritten silently", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().LogAll(mock.Anything, mock.Anything, "workspace/forge-factory.yaml").
			Return([]string{shaB}, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaB, "workspace/forge-factory.yaml").
			Return(oldCommittedYaml, true, nil)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = oldCommittedYaml

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.NoError(t, err)
		require.Equal(t, claimingFactoryYaml, r.fs.writes[dest])
	})

	t.Run("a hand edited file is refused with neither flag", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().LogAll(mock.Anything, mock.Anything, "workspace/forge-factory.yaml").
			Return([]string{shaB}, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaB, "workspace/forge-factory.yaml").
			Return(oldCommittedYaml, true, nil)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.ErrorIs(t, err, ErrProtectedFile)
		require.ErrorContains(t, err, "--backup")
		require.ErrorContains(t, err, "--force")

		_, written := r.fs.writes[dest]
		require.False(t, written, "a refused write touches nothing")
	})

	t.Run("--backup keeps the hand edited file as .bak and writes the new one", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().LogAll(mock.Anything, mock.Anything, "workspace/forge-factory.yaml").
			Return([]string{shaB}, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaB, "workspace/forge-factory.yaml").
			Return(oldCommittedYaml, true, nil)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml
		r.fs.writes[dest] = handEditedYaml

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true, Backup: true,
		})
		require.NoError(t, err)
		require.Equal(t, handEditedYaml, r.fs.writes[dest+".bak"])
		require.Equal(t, claimingFactoryYaml, r.fs.writes[dest])
	})

	t.Run("--force overwrites the hand edited file with no backup", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true, Force: true,
		})
		require.NoError(t, err)
		require.Equal(t, claimingFactoryYaml, r.fs.writes[dest])

		_, hasBackup := r.fs.writes[dest+".bak"]
		require.False(t, hasBackup, "--force keeps no backup")
	})

	t.Run("an unreadable placed file's existence check fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.existsErr[dest] = errors.New("stat refused")

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.ErrorContains(t, err, "stat refused")
	})

	t.Run("an unreadable placed file fails the comparison", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml
		r.fs.readErr[dest] = errors.New("read refused")

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.ErrorContains(t, err, "read refused")
	})

	t.Run("a history lookup that cannot resolve fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().LogAll(mock.Anything, mock.Anything, "workspace/forge-factory.yaml").
			Return(nil, errors.New("git log refused"))

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.ErrorContains(t, err, "reading the history of workspace/forge-factory.yaml")
		require.ErrorContains(t, err, "git log refused")
	})

	t.Run("a committed version that cannot be shown fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().LogAll(mock.Anything, mock.Anything, "workspace/forge-factory.yaml").
			Return([]string{shaB}, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaB, "workspace/forge-factory.yaml").
			Return("", false, errors.New("git show refused"))

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true,
		})
		require.ErrorContains(t, err, "git show refused")
	})

	t.Run("--backup fails when the old file cannot be moved aside", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().LogAll(mock.Anything, mock.Anything, "workspace/forge-factory.yaml").
			Return([]string{shaB}, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaB, "workspace/forge-factory.yaml").
			Return(oldCommittedYaml, true, nil)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml
		r.fs.renameErr[dest] = errors.New("rename refused")

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true, Backup: true,
		})
		require.ErrorContains(t, err, "backing up "+dest)
		require.ErrorContains(t, err, "rename refused")
	})

	t.Run("--backup refuses to clobber a prior backup", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().LogAll(mock.Anything, mock.Anything, "workspace/forge-factory.yaml").
			Return([]string{shaB}, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaB, "workspace/forge-factory.yaml").
			Return(oldCommittedYaml, true, nil)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml
		r.fs.files[dest+".bak"] = "an earlier backup, from an earlier refused bootstrap\n"

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true, Backup: true,
		})
		require.ErrorIs(t, err, ErrProtectedFile)
		require.ErrorContains(t, err, dest+".bak already exists, move it away first")

		_, written := r.fs.writes[dest]
		require.False(t, written, "a refused backup touches nothing")
	})

	t.Run("--backup fails when checking for a prior backup fails", func(t *testing.T) {
		r := newRig(t)
		factoryFlow(r)
		r.git.EXPECT().LogAll(mock.Anything, mock.Anything, "workspace/forge-factory.yaml").
			Return([]string{shaB}, nil)
		r.git.EXPECT().Show(mock.Anything, mock.Anything, shaB, "workspace/forge-factory.yaml").
			Return(oldCommittedYaml, true, nil)

		dir := t.TempDir()
		dest := filepath.Join(dir, "forge-factory.yaml")
		r.fs.files[dest] = handEditedYaml
		r.fs.existsErr[dest+".bak"] = errors.New("stat refused")

		_, _, err := r.c.Bootstrap(ctx, BootstrapRequest{
			Factory: factoryURL, Dir: dir, CacheDir: t.TempDir(), Quiet: true, Backup: true,
		})
		require.ErrorContains(t, err, "stat refused")
	})
}

// nopLocker is what a unit rig locks with. The real lock writes a file
// beside every directory a controller names, so wiring it by default made
// unit tests depend on the privileges of whatever path a fixture invented
// - literal "/clone" passed for years as an opaque name and then started
// failing for everyone who is not root. Serialization is proved where it
// is the subject (the lockadapter suite, and the concurrent
// materialisation test), not incidentally everywhere else.
type nopLocker struct{}

func (nopLocker) Lock(string) (func(), error) { return func() {}, nil }

// withRealLock swaps in the file lock for the subtests whose subject IS
// the lock's failure on an unusable path.
func (r *rig) withRealLock() {
	r.c = New(r.fs, r.git, r.exec, r.sync, lockadapter.New(), r.out)
}
