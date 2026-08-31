package clidriver_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/clonecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/revisioncontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/speccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/statuscontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/toolingcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/driver/clidriver"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/clonecontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/fsadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/revisioncontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/runcontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/statuscontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/synccontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/toolingcontrollermock"
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

type harness struct {
	out      *bytes.Buffer
	fs       *fsadaptermock.MockFS
	clone    *clonecontrollermock.MockCloner
	sync     *synccontrollermock.MockSyncer
	revise   *revisioncontrollermock.MockReviser
	state    *statuscontrollermock.MockStater
	runner   *runcontrollermock.MockRunner
	tooling  *toolingcontrollermock.MockApplier
	driver   *clidriver.Driver
	wrote    map[string]string
	exitCode *int
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		out:     &bytes.Buffer{},
		fs:      fsadaptermock.NewMockFS(t),
		clone:   clonecontrollermock.NewMockCloner(t),
		sync:    synccontrollermock.NewMockSyncer(t),
		revise:  revisioncontrollermock.NewMockReviser(t),
		state:   statuscontrollermock.NewMockStater(t),
		runner:  runcontrollermock.NewMockRunner(t),
		tooling: toolingcontrollermock.NewMockApplier(t),
		wrote:   map[string]string{},
	}

	h.driver = clidriver.New(h.out, h.fs, h.clone, h.sync, h.revise, h.state, h.runner, h.tooling,
		func(code int) {
			if h.exitCode != nil {
				*h.exitCode = code
			}
		})

	return h
}

func (h *harness) reads(raw string) {
	h.fs.EXPECT().ReadFile("forge-factory.yaml").Return([]byte(raw), nil).Maybe()
}

func (h *harness) recordWrites() {
	h.fs.EXPECT().WriteFile(mock.Anything, mock.Anything).
		RunAndReturn(func(path string, data []byte) error {
			h.wrote[path] = string(data)

			return nil
		}).Once()
}

func (h *harness) expectSync() {
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{Root: "/w", Written: []string{"/w/go.work"}}, nil).Once()
}

func TestRunWithNoVerb(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	require.ErrorIs(t, h.driver.Run(t.Context(), nil), clidriver.ErrUsage)
	assert.Contains(t, clidriver.Usage(), "forge-factory")
}

func TestRunWithAnUnknownVerb(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"deploy"}), clidriver.ErrUsage)
}

func TestValidateDescribesTheFactory(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	require.NoError(t, h.driver.Run(t.Context(), []string{"validate"}))
	assert.Contains(t, h.out.String(), "golden: 1 repos, 1 engines, 1 languages")
	assert.Contains(t, h.out.String(), "go forge://example.com/lang-go (1 dependencies)")
}

func TestSyncPrintsWhatItWrote(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{
			Root:    "/w",
			Written: []string{"/w/go.work", "/w/golden-go/go.mod"},
			Ignored: []string{"/w/golden-go/.gitignore"},
		}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"sync", "--root", "/w"}))
	assert.Contains(t, h.out.String(), "wrote go.work")
	assert.Contains(t, h.out.String(), "wrote golden-go/go.mod")
	assert.Contains(t, h.out.String(), "ignored in golden-go/.gitignore")
}

// The sync verb consumes a distribution when a source is named: the flag
// form here, the FORGE_DIST_MIRROR environment form below - the airgap
// door, where the mirrored release assets are the bundle.
func TestSyncConsumesADistributionWhenAsked(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.expectSync()
	h.tooling.EXPECT().Apply(mock.MatchedBy(func(req toolingcontroller.Request) bool {
		return req.Root == "/w" && req.SourceName == "/mirror" && req.Source != nil
	})).Return(toolingcontroller.Report{
		Revision: "abc123def456", Platform: "linux/amd64",
		Installed: []string{"forge"}, Reused: []string{"forge-factory"},
		BinDir: "/w/.forge/bin",
	}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(),
		[]string{"sync", "--root", "/w", "--tooling-from", "/mirror"}))
	assert.Contains(t, h.out.String(), "tooling abc123def456 (linux/amd64)")
	assert.Contains(t, h.out.String(), "installed forge")
	assert.Contains(t, h.out.String(), "reused forge-factory")
	assert.Contains(t, h.out.String(), "linked /w/.forge/bin")
}

func TestSyncConsumesTheMirrorNamedByTheEnvironment(t *testing.T) {
	t.Setenv("FORGE_DIST_MIRROR", "/mirror-from-env")

	h := newHarness(t)
	h.reads(factory)
	h.expectSync()
	h.tooling.EXPECT().Apply(mock.MatchedBy(func(req toolingcontroller.Request) bool {
		return req.SourceName == "/mirror-from-env"
	})).Return(toolingcontroller.Report{Revision: "abc123def456"}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"sync", "--root", "/w"}))
	assert.Contains(t, h.out.String(), "tooling abc123def456")
}

// Toolchain binaries sync resolved are provisioned into the store, and
// the report says what was built and what was reused.
func TestSyncProvisionsTheResolvedToolchainBinaries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	binaries := []toolingcontroller.Binary{
		{Name: "mockery", Module: "github.com/vektra/mockery/v3", Version: "v3.5.5"},
	}

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{Root: "/w", Toolchain: binaries}, nil).Once()
	h.tooling.EXPECT().ProvisionBinaries(mock.Anything, "/w", "", binaries).
		Return(toolingcontroller.BinaryReport{Installed: []string{"mockery"}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"sync", "--root", "/w"}))
	assert.Contains(t, h.out.String(), "toolchain binaries")
	assert.Contains(t, h.out.String(), "installed mockery")
}

func TestAFailingDistributionFailsTheSync(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.expectSync()
	h.tooling.EXPECT().Apply(mock.Anything).
		Return(toolingcontroller.Report{}, assert.AnError).Once()

	err := h.driver.Run(t.Context(), []string{"sync", "--root", "/w", "--tooling-from", "/m"})
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "consuming the distribution from /m")
}

func TestSyncReportsAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{}, assert.AnError).Once()

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"sync"}), assert.AnError)
}

func TestStatusFailsWhenTheWorkspaceDisagrees(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		statuscontroller.Report{
			Root: "/w",
			Repos: []statuscontroller.RepoStatus{
				{Name: "a"},
				{Name: "b", Present: true},
				{Name: "c", Present: true, Cloned: true, Dirty: true, Head: "aaaaaaaaaaaaaaaa"},
				{Name: "d", Present: true, Cloned: true, Head: "bbbbbbbbbbbbbbbb"},
			},
			Unknown: []string{"stray"},
		}, nil).Once()

	err := h.driver.Run(t.Context(), []string{"status"})
	require.ErrorIs(t, err, clidriver.ErrDrift)

	got := h.out.String()
	assert.Contains(t, got, "a is missing")
	assert.Contains(t, got, "b is a directory and not a git repo")
	assert.Contains(t, got, "c aaaaaaaaaaaa dirty")
	assert.Contains(t, got, "d bbbbbbbbbbbb")
	assert.Contains(t, got, "stray is a repo the factory does not declare")
}

func TestStatusPassesWhenEverythingAgrees(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		statuscontroller.Report{Root: "/w", Repos: []statuscontroller.RepoStatus{
			{Name: "a", Present: true, Cloned: true, Head: "aaa"},
		}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"status"}))
}

func TestStatusReportsAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(statuscontroller.Report{}, assert.AnError).Once()

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"status"}), assert.AnError)
}

func TestBumpRewritesTheFileAndSyncs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.recordWrites()
	h.expectSync()

	// A bump proves its own version: sync writes the manifests, and the
	// lock is what fails when the new version resolves nowhere.
	h.sync.EXPECT().Lock(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{Root: "/w"}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"bump", "sigs.k8s.io/yaml", "v1.7.0"}))
	assert.Contains(t, h.wrote["forge-factory.yaml"], "sigs.k8s.io/yaml: v1.7.0")
	assert.Contains(t, h.out.String(), "was sigs.k8s.io/yaml: v1.6.0")
	assert.Contains(t, h.out.String(), "now sigs.k8s.io/yaml: v1.7.0")
}

func TestBumpReportsAMissingDependencyWhenTheFactoryDeclaresNone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fs.EXPECT().ReadFile("forge-factory.yaml").
		Return([]byte(strings.Split(factory, "dependencies:")[0]), nil).Twice()

	err := h.driver.Run(t.Context(), []string{"bump", "example.com/nope", "v1"})
	require.ErrorIs(t, err, speccontroller.ErrNotFound)
	assert.NotContains(t, err.Error(), "the factory declares")
}

func TestBumpNeedsBothANameAndAVersion(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"bump", "only-one"}), clidriver.ErrUsage)
}

func TestBumpReportsADependencyNobodyDeclares(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	err := h.driver.Run(t.Context(), []string{"bump", "example.com/nope", "v1"})
	require.ErrorIs(t, err, speccontroller.ErrNotFound)
	assert.Contains(t, err.Error(), "the factory declares: go:sigs.k8s.io/yaml")
}

func TestAddAppendsAMemberAndSyncs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.recordWrites()
	h.expectSync()

	err := h.driver.Run(t.Context(), []string{"add", "golden-two", "git@x:y.git", "go"})
	require.NoError(t, err)
	assert.Contains(t, h.wrote["forge-factory.yaml"], "golden-two")
	assert.Contains(t, h.out.String(), "added")
}

func TestAddNeedsANameAURLAndALanguage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	err := h.driver.Run(t.Context(), []string{"add", "golden-two", "git@x:y.git"})
	require.ErrorIs(t, err, clidriver.ErrUsage)
}

func TestAddReportsAMemberAlreadyDeclared(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	err := h.driver.Run(t.Context(), []string{"add", "golden-go", "u", "go"})
	require.ErrorIs(t, err, speccontroller.ErrExists)
}

func TestCheckoutPutsMembersOnTheirSHAsThenSyncs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.revise.EXPECT().Checkout(mock.Anything, mock.Anything, mock.Anything, "abc123").Return(
		revisioncontroller.Result{
			Revision: "abc123",
			Repos:    map[string]string{"golden-go": "aaaaaaaaaaaaaaaa", "golden-rs": "bbb"},
		}, nil).Once()
	h.expectSync()

	require.NoError(t, h.driver.Run(t.Context(), []string{"checkout", "abc123"}))

	got := h.out.String()
	assert.Contains(t, got, "revision abc123")
	assert.Contains(t, got, "golden-go aaaaaaaaaaaa")
	assert.Less(t, strings.Index(got, "golden-go"), strings.Index(got, "golden-rs"),
		"members print in a stable order")
}

func TestCheckoutNeedsOneRevision(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"checkout"}), clidriver.ErrUsage)
}

func TestCheckoutReportsAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.revise.EXPECT().Checkout(mock.Anything, mock.Anything, mock.Anything, "abc").
		Return(revisioncontroller.Result{}, assert.AnError).Once()

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"checkout", "abc"}), assert.AnError)
}

func TestRunReportsAFactoryThatCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fs.EXPECT().ReadFile("forge-factory.yaml").Return(nil, assert.AnError).Once()

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"sync"}), assert.AnError)
}

func TestRunReportsAFactoryThatDoesNotParse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fs.EXPECT().ReadFile("forge-factory.yaml").Return([]byte("name: x\n"), nil).Once()

	err := h.driver.Run(t.Context(), []string{"sync"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading forge-factory.yaml")
}

func TestRunReportsABadFlag(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	err := h.driver.Run(t.Context(), []string{"sync", "--nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing flags")
}

func TestRootDefaultsToTheFactoryFilesParent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fs.EXPECT().ReadFile("sub/forge-factory.yaml").Return([]byte(factory), nil).Once()

	var seen string

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ config.Factory, root string, _ string) (synccontroller.Report, error) {
			seen = root

			return synccontroller.Report{Root: root}, nil
		}).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"sync", "--config", "sub/forge-factory.yaml"}))
	assert.True(t, strings.HasSuffix(seen, "/sub"), "got %q", seen)
}

func TestAWriteThatFailsIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.fs.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(assert.AnError).Once()

	err := h.driver.Run(t.Context(), []string{"bump", "sigs.k8s.io/yaml", "v1.7.0"})
	require.ErrorIs(t, err, assert.AnError)
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, assert.AnError }

func TestAReportThatCannotBePrintedIsAnError(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().ReadFile("forge-factory.yaml").Return([]byte(factory), nil).Once()

	driver := clidriver.New(brokenWriter{}, fs,
		clonecontrollermock.NewMockCloner(t),
		synccontrollermock.NewMockSyncer(t), revisioncontrollermock.NewMockReviser(t),
		statuscontrollermock.NewMockStater(t), nil, nil, func(int) {})

	err := driver.Run(t.Context(), []string{"validate"})
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "writing report")
}

func TestAPathOutsideTheRootPrintsWhole(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{Root: "relative", Written: []string{"/absolute/go.work"}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"sync", "--root", "relative"}))
	assert.Contains(t, h.out.String(), "/absolute/go.work")
}

// lock is its own verb: sync writes manifests and stops, and this is what
// resolves the closure. A lock that could not resolve leaves every member
// unbuildable, so reporting it and exiting zero would be a lie.
func TestALockThatCouldNotResolveFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Lock(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{
			Root:     "/w",
			Unlocked: []string{"go mod tidy in /w/a: unknown revision v1.7.0"},
		}, nil).Once()

	err := h.driver.Run(t.Context(), []string{"lock"})
	require.ErrorIs(t, err, clidriver.ErrUnlocked)
	assert.Contains(t, h.out.String(), "which a build will need")
	assert.Contains(t, err.Error(), "unknown revision v1.7.0")
}

func TestLockPrintsWhatItRan(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Lock(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{Root: "/w", Locked: []string{"go mod tidy in /w/a"}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"lock", "--root", "/w"}))
	assert.Contains(t, h.out.String(), "ran go mod tidy in /w/a")
}

func TestALockReportThatCannotBePrintedIsAnError(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().ReadFile("forge-factory.yaml").Return([]byte(factory), nil).Once()

	sync := synccontrollermock.NewMockSyncer(t)
	sync.EXPECT().Lock(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{Root: "/w"}, nil).Once()

	driver := clidriver.New(brokenWriter{}, fs,
		clonecontrollermock.NewMockCloner(t), sync,
		revisioncontrollermock.NewMockReviser(t), statuscontrollermock.NewMockStater(t),
		nil, nil, func(int) {})

	err := driver.Run(t.Context(), []string{"lock"})
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "writing report")
}

func TestALockThatBreaksIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Lock(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{}, assert.AnError).Once()

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"lock"}), assert.AnError)
}

func TestOfflineAllowsALockThatCouldNotReachTheNetwork(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Lock(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{Root: "/w", Unlocked: []string{"go mod tidy in /w/a: no network"}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"lock", "--offline"}))
}

func TestCloneFetchesTheMembersThenSyncs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.clone.EXPECT().Clone(mock.Anything, mock.Anything, mock.Anything).Return(
		clonecontroller.Report{Root: "/w", Cloned: []string{"golden-go"}}, nil).Once()
	h.expectSync()

	require.NoError(t, h.driver.Run(t.Context(), []string{"clone"}))
	assert.Contains(t, h.out.String(), "cloned golden-go")
	assert.Contains(t, h.out.String(), "wrote go.work",
		"a member with no manifest is not yet something forge can build")
}

func TestCloneSaysSoWhenNothingWasMissing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.clone.EXPECT().Clone(mock.Anything, mock.Anything, mock.Anything).Return(
		clonecontroller.Report{Root: "/w", Present: []string{"golden-go"}}, nil).Once()
	h.expectSync()

	require.NoError(t, h.driver.Run(t.Context(), []string{"clone"}))
	assert.Contains(t, h.out.String(), "every member was already there")
}

func TestCloneReportsAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.clone.EXPECT().Clone(mock.Anything, mock.Anything, mock.Anything).
		Return(clonecontroller.Report{}, assert.AnError).Once()

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"clone"}), assert.AnError)
}

func TestSyncPrintsTheResolverNotes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{
			Root:  "/w",
			Notes: []string{"soft pin go:x v1 is behind track 1 (v2) - the register is newer; remove this pin"},
		}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"sync", "--root", "/w"}))
	assert.Contains(t, h.out.String(), "note: soft pin go:x")
}

func TestAddReportsAFailedWrite(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	failed := errors.New("disk full")
	h.fs.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(failed).Once()

	err := h.driver.Run(t.Context(), []string{"add", "golden-two", "git@x:y.git", "go"})
	require.ErrorIs(t, err, failed)
}

func TestRegisterHeadClearsThePinnedRevision(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory + `register:
  url: git@example.com:golden-register.git
  revision: v0.1.0
`)

	var saw config.Factory

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, f config.Factory, _ string, _ string) (synccontroller.Report, error) {
			saw = f

			return synccontroller.Report{Root: "/w"}, nil
		}).Once()

	require.NoError(t, h.driver.Run(t.Context(),
		[]string{"sync", "--root", "/w", "--register-head"}))
	require.NotNil(t, saw.Register)
	assert.Empty(t, saw.Register.Revision,
		"the canary must see the candidate index, not the published tag")
}

func TestSyncPrunePinsRewritesTheFactoryAndSyncsAgain(t *testing.T) {
	t.Parallel()

	pinned := `version: "1"
name: golden
repos:
  - name: golden-go
    url: u
    languages: [go]
register:
  url: git@example.com:golden-register.git
dependencies:
  go:
    example.com/pkg: { track: "1", pin: v1.5.0, mode: soft, reason: dead }
engines:
  - alias: go
    engine: forge://example.com/lang-go
`

	h := newHarness(t)
	h.reads(pinned)
	h.recordWrites()

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{Root: "/w", Notes: []string{
			"soft pin go:example.com/pkg v1.5.0 is behind track 1 (v1.6.0) - the register is newer; remove this pin",
		}}, nil).Once()
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, f config.Factory, _ string, _ string) (synccontroller.Report, error) {
			require.Empty(t, f.Dependencies["go"]["example.com/pkg"].Pin,
				"the re-sync runs on the pruned factory")

			return synccontroller.Report{Root: "/w"}, nil
		}).Once()

	require.NoError(t, h.driver.Run(t.Context(),
		[]string{"sync", "--root", "/w", "--prune-pins"}))
	assert.Contains(t, h.out.String(), "pruned a dead pin")
	assert.Contains(t, h.wrote["forge-factory.yaml"], `{"track":"1"}`)
}

func TestSyncPrunePinsWithNothingDeadSyncsOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{Root: "/w"}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(),
		[]string{"sync", "--root", "/w", "--prune-pins"}))
	assert.NotContains(t, h.out.String(), "pruned")
}

func TestStatusRendersFreshness(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything, false).Return(
		statuscontroller.Report{Root: "/w", Repos: []statuscontroller.RepoStatus{
			{
				Name: "a", Present: true, Cloned: true, Head: "aaa111",
				Freshness: statuscontroller.Ahead, Ahead: 2,
			},
			{
				Name: "b", Present: true, Cloned: true, Head: "bbb222",
				Freshness: statuscontroller.Behind, Behind: 3,
			},
			{
				Name: "c", Present: true, Cloned: true, Head: "ccc333", Dirty: true,
				Freshness: statuscontroller.Diverged, Ahead: 1, Behind: 2,
			},
		}}, nil).Once()

	err := h.driver.Run(t.Context(), []string{"status", "--root", "/w"})
	require.ErrorIs(t, err, clidriver.ErrDrift, "a diverged checkout fails status")
	assert.Contains(t, h.out.String(), "2 ahead of origin/main")
	assert.Contains(t, h.out.String(), "3 behind origin/main - pull it")
	assert.Contains(t, h.out.String(), "diverged: 1 ahead, 2 behind")
	assert.Contains(t, h.out.String(), "ccc333 dirty (diverged")
}

func TestStatusOfflineSkipsFreshnessAndSaysSo(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything, true).Return(
		statuscontroller.Report{Root: "/w", Offline: true, Repos: []statuscontroller.RepoStatus{
			{Name: "a", Present: true, Cloned: true, Head: "aaa111"},
		}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"status", "--root", "/w", "--offline"}))
	assert.Contains(t, h.out.String(), "freshness skipped (--offline)")
}

func TestPrunePinsReportsADependencyTheFactoryLost(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{Root: "/w", Notes: []string{
			"soft pin go:example.com/ghost v1 is behind track 1 (v2) - the register is newer; remove this pin",
		}}, nil).Once()

	err := h.driver.Run(t.Context(), []string{"sync", "--root", "/w", "--prune-pins"})
	require.ErrorIs(t, err, speccontroller.ErrNotFound)
}

func TestPrunePinsReportsAFailedWrite(t *testing.T) {
	t.Parallel()

	pinned := `version: "1"
name: golden
repos:
  - name: golden-go
    url: u
    languages: [go]
register:
  url: git@example.com:golden-register.git
dependencies:
  go:
    example.com/pkg: { pin: v1.5.0, mode: soft, reason: dead }
engines:
  - alias: go
    engine: forge://example.com/lang-go
`

	h := newHarness(t)
	h.reads(pinned)
	failed := errors.New("disk full")
	h.fs.EXPECT().WriteFile(mock.Anything, mock.Anything).Return(failed).Once()
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{Root: "/w", Notes: []string{
			"soft pin go:example.com/pkg v1.5.0 is behind track 1 (v1.6.0) - the register is newer; remove this pin",
		}}, nil).Once()

	err := h.driver.Run(t.Context(), []string{"sync", "--root", "/w", "--prune-pins"})
	require.ErrorIs(t, err, failed)
}

func TestRunSplitsFlagsTargetAndProgramArgs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	var got runcontroller.Request

	h.runner.EXPECT().Run(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, req runcontroller.Request) (int, error) {
			got = req

			return 0, nil
		}).Once()

	require.NoError(t, h.driver.Run(t.Context(),
		[]string{"run", "--factory", "git@x:f.git@v1", "--quiet", "my-tool", "--", "-v", "serve"}))
	assert.Equal(t, "my-tool", got.Target)
	assert.Equal(t, []string{"-v", "serve"}, got.Args)
	assert.Equal(t, "git@x:f.git@v1", got.Factory)
	assert.True(t, got.Quiet)
	assert.NotEmpty(t, got.WorkDir)
}

func TestRunTakesARepoAndARunnableName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	var got runcontroller.Request

	h.runner.EXPECT().Run(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, req runcontroller.Request) (int, error) {
			got = req

			return 0, nil
		}).Once()

	require.NoError(t, h.driver.Run(t.Context(),
		[]string{"run", "github.com/x/repo", "my-tool"}))
	assert.Equal(t, "github.com/x/repo", got.Target)
	assert.Equal(t, "my-tool", got.Name)
}

func TestRunPropagatesANonZeroExitCode(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.runner.EXPECT().Run(mock.Anything, mock.Anything).Return(3, nil).Once()

	code := -1
	h.exitCode = &code

	require.NoError(t, h.driver.Run(t.Context(), []string{"run", "my-tool"}))
	assert.Equal(t, 3, code)
}

func TestRunWithNoTargetIsUsage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	err := h.driver.Run(t.Context(), []string{"run"})
	require.ErrorIs(t, err, clidriver.ErrUsage)

	err = h.driver.Run(t.Context(), []string{"run", "a", "b", "c"})
	require.ErrorIs(t, err, clidriver.ErrUsage)
}

func TestBootstrapPlacesThenClonesThenSyncs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	f, err := config.Parse([]byte(factory))
	require.NoError(t, err)

	h.runner.EXPECT().Bootstrap(mock.Anything, runcontroller.BootstrapRequest{
		Factory: "git@x:f.git", Dir: "dest",
	}).Return(f, "/abs/dest", nil).Once()
	h.clone.EXPECT().Clone(mock.Anything, mock.Anything, "/abs/dest").
		Return(clonecontroller.Report{Root: "/abs/dest"}, nil).Once()
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, "/abs/dest", mock.Anything).
		Return(synccontroller.Report{Root: "/abs/dest"}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(),
		[]string{"bootstrap", "git@x:f.git", "dest"}))
	assert.Contains(t, h.out.String(), "synced /abs/dest")
}

func TestBootstrapNeedsAFactoryURL(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"bootstrap"}), clidriver.ErrUsage)
}

// cache clean is the one last-resort verb: everything under the cache is
// derived, so removing it is always safe and the next run rebuilds it.
func TestCacheCleanRemovesTheCacheDir(t *testing.T) {
	h := newHarness(t)

	dir := filepath.Join(t.TempDir(), "forge-factory")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "run", "some-root"), 0o750))

	h.fs.EXPECT().IsDir(dir).Return(true, nil).Once()

	require.NoError(t, h.driver.Run(context.Background(), []string{"cache", "clean", "--cache", dir}))
	require.NoDirExists(t, dir)
	require.Contains(t, h.out.String(), "removed "+dir)
}

func TestCacheCleanOnNothingSaysSo(t *testing.T) {
	h := newHarness(t)

	dir := filepath.Join(t.TempDir(), "absent")
	h.fs.EXPECT().IsDir(dir).Return(false, nil).Once()

	require.NoError(t, h.driver.Run(context.Background(), []string{"cache", "clean", "--cache", dir}))
	require.Contains(t, h.out.String(), "nothing at "+dir)
}

func TestCacheWithoutCleanIsAUsageError(t *testing.T) {
	h := newHarness(t)

	err := h.driver.Run(context.Background(), []string{"cache"})
	require.ErrorContains(t, err, "usage: forge-factory cache clean")
}

func TestCacheRefusesAnythingButClean(t *testing.T) {
	// clean is the only verb, and it deletes a directory. A misspelling that
	// fell through to the default would delete the cache the user did not ask
	// to delete.
	for _, args := range [][]string{
		{"cache", "wipe"},
		{"cache", "clean", "extra"},
	} {
		h := newHarness(t)
		require.ErrorContains(t, h.driver.Run(context.Background(), args),
			"usage: forge-factory cache clean")
	}
}

func TestCacheRefusesAnUnknownFlag(t *testing.T) {
	h := newHarness(t)

	// An ignored flag makes the operator think they cleaned a directory they
	// did not clean.
	require.Error(t, h.driver.Run(context.Background(), []string{"cache", "clean", "--nope"}))
}

func TestCacheFallsBackToTheUserCache(t *testing.T) {
	h := newHarness(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	dir := filepath.Join(home, "cache", "forge-factory")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	h.fs.EXPECT().IsDir(dir).Return(true, nil).Once()

	require.NoError(t, h.driver.Run(context.Background(), []string{"cache", "clean"}))
	require.NoDirExists(t, dir)
}

func TestCacheSaysSoWhenThereIsNoUserCache(t *testing.T) {
	h := newHarness(t)

	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	err := h.driver.Run(context.Background(), []string{"cache", "clean"})
	require.ErrorContains(t, err, "finding the cache directory")
}

// --root arrives verbatim from a CI recipe, and ".." is the form the recipe
// prints. It used to reach go work sync as GOWORK=../go.work, which go
// refuses outright, so the sync reported the workspace unbuildable and
// exited before provisioning any tooling.
func TestARelativeRootIsMadeAbsolute(t *testing.T) {
	h := newHarness(t)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "member"), 0o750))
	h.fs.EXPECT().ReadFile("../forge-factory.yaml").Return([]byte(factory), nil).Once()

	var seen string

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ config.Factory, root, _ string) (synccontroller.Report, error) {
			seen = root

			return synccontroller.Report{Root: root}, nil
		}).Once()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(filepath.Join(dir, "member")))

	t.Cleanup(func() { _ = os.Chdir(cwd) })

	require.NoError(t, h.driver.Run(context.Background(),
		[]string{"sync", "--config", "../forge-factory.yaml", "--root", ".."}))

	require.True(t, filepath.IsAbs(seen), "the controller was handed %q", seen)
}
