package clidriver_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/clonecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/revisioncontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/speccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/statuscontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/driver/clidriver"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/clonecontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/fsadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/revisioncontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/statuscontrollermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/synccontrollermock"
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

type harness struct {
	out    *bytes.Buffer
	fs     *fsadaptermock.MockFS
	clone  *clonecontrollermock.MockCloner
	sync   *synccontrollermock.MockSyncer
	revise *revisioncontrollermock.MockReviser
	state  *statuscontrollermock.MockStater
	driver *clidriver.Driver
	wrote  map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		out:    &bytes.Buffer{},
		fs:     fsadaptermock.NewMockFS(t),
		clone:  clonecontrollermock.NewMockCloner(t),
		sync:   synccontrollermock.NewMockSyncer(t),
		revise: revisioncontrollermock.NewMockReviser(t),
		state:  statuscontrollermock.NewMockStater(t),
		wrote:  map[string]string{},
	}

	h.driver = clidriver.New(h.out, h.fs, h.clone, h.sync, h.revise, h.state)

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
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything).
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
	assert.Contains(t, h.out.String(), "go go://example.com/lang-go (1 dependencies)")
}

func TestSyncPrintsWhatItWrote(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything).Return(
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

func TestSyncReportsAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything).
		Return(synccontroller.Report{}, assert.AnError).Once()

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"sync"}), assert.AnError)
}

func TestStatusFailsWhenTheWorkspaceDisagrees(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything).Return(
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
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything).Return(
		statuscontroller.Report{Root: "/w", Repos: []statuscontroller.RepoStatus{
			{Name: "a", Present: true, Cloned: true, Head: "aaa"},
		}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"status"}))
}

func TestStatusReportsAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything).
		Return(statuscontroller.Report{}, assert.AnError).Once()

	require.ErrorIs(t, h.driver.Run(t.Context(), []string{"status"}), assert.AnError)
}

func TestBumpRewritesTheFileAndSyncs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.recordWrites()
	h.expectSync()

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

	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ config.Factory, root string) (synccontroller.Report, error) {
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
		statuscontrollermock.NewMockStater(t))

	err := driver.Run(t.Context(), []string{"validate"})
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "writing report")
}

func TestAPathOutsideTheRootPrintsWhole(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{Root: "relative", Written: []string{"/absolute/go.work"}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"sync", "--root", "relative"}))
	assert.Contains(t, h.out.String(), "/absolute/go.work")
}

func TestASyncThatLeavesTheWorkspaceUnbuildableFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{
			Root:      "/w",
			Unsettled: []string{"go mod tidy in /w/a: unknown revision v1.7.0"},
		}, nil).Once()

	err := h.driver.Run(t.Context(), []string{"sync"})
	require.ErrorIs(t, err, clidriver.ErrUnsettled)
	assert.Contains(t, h.out.String(), "which a build will need")
	assert.Contains(t, err.Error(), "unknown revision v1.7.0")
}

func TestOfflineAllowsASyncThatCouldNotReachTheNetwork(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Sync(mock.Anything, mock.Anything, mock.Anything).Return(
		synccontroller.Report{Root: "/w", Unsettled: []string{"go mod tidy in /w/a: no network"}}, nil).Once()

	require.NoError(t, h.driver.Run(t.Context(), []string{"sync", "--offline"}))
}

func TestStatusNamesAPinThatFellBehind(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.state.EXPECT().Status(mock.Anything, mock.Anything, mock.Anything).Return(
		statuscontroller.Report{
			Root: "/w",
			Modules: []statuscontroller.ModuleStatus{
				{Path: "github.com/x/spec", Pinned: "v0.1.0", Latest: "v0.3.0"},
				{Path: "github.com/x/other", Pinned: "v1.0.0", Latest: "v1.0.0"},
			},
		}, nil).Once()

	err := h.driver.Run(t.Context(), []string{"status"})
	require.ErrorIs(t, err, clidriver.ErrDrift)
	assert.Contains(t, h.out.String(), "github.com/x/spec is pinned at v0.1.0 and the checkout carries v0.3.0")
	assert.NotContains(t, h.out.String(), "github.com/x/other")
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
