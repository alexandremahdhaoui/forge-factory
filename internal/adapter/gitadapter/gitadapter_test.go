package gitadapter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var errBoom = errors.New("boom")

func ok(stdout string) execadapter.Result { return execadapter.Result{Stdout: stdout} }

func TestInitCreatesAMainBranch(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "init", "-b", "main").Return(ok(""), nil).Once()

	require.NoError(t, gitadapter.New(r).Init(context.Background(), "/repo"))
}

func TestCloneRunsFromNowhereInParticular(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "", "git", "clone", "git@x:y.git", "/repo").Return(ok(""), nil).Once()

	require.NoError(t, gitadapter.New(r).Clone(context.Background(), "git@x:y.git", "/repo"))
}

func TestIsRepoReadsTheExitCode(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "rev-parse", "--git-dir").Return(ok(".git"), nil).Once()

	yes, err := gitadapter.New(r).IsRepo(context.Background(), "/repo")
	require.NoError(t, err)
	require.True(t, yes)

	r2 := execadaptermock.NewMockRunner(t)
	r2.EXPECT().Run(mock.Anything, "/x", "git", "rev-parse", "--git-dir").
		Return(execadapter.Result{ExitCode: 128}, nil).Once()

	no, err := gitadapter.New(r2).IsRepo(context.Background(), "/x")
	require.NoError(t, err)
	require.False(t, no)
}

func TestIsRepoReportsABrokenRunner(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, mock.Anything, "git", "rev-parse", "--git-dir").
		Return(execadapter.Result{}, errBoom).Once()

	_, err := gitadapter.New(r).IsRepo(context.Background(), "/repo")
	require.ErrorIs(t, err, errBoom)
}

func TestHeadSHAIsTrimmed(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "rev-parse", "HEAD").Return(ok("abc123\n"), nil).Once()

	sha, err := gitadapter.New(r).HeadSHA(context.Background(), "/repo")
	require.NoError(t, err)
	require.Equal(t, "abc123", sha)
}

func TestRemoteSHATakesTheFirstField(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "", "git", "ls-remote", "git@x:y.git", "refs/heads/main").
		Return(ok("abc123\trefs/heads/main\n"), nil).Once()

	sha, err := gitadapter.New(r).RemoteSHA(context.Background(), "git@x:y.git", "")
	require.NoError(t, err)
	require.Equal(t, "abc123", sha)
}

func TestRemoteSHAWithNoSuchRef(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "", "git", "ls-remote", "git@x:y.git", "refs/heads/nope").
		Return(ok(""), nil).Once()

	_, err := gitadapter.New(r).RemoteSHA(context.Background(), "git@x:y.git", "nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no such ref")
}

func TestDirtyIsTrueWhenStatusPrintsAnything(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "status", "--porcelain").Return(ok(" M f.go\n"), nil).Once()

	dirty, err := gitadapter.New(r).Dirty(context.Background(), "/repo")
	require.NoError(t, err)
	require.True(t, dirty)

	r2 := execadaptermock.NewMockRunner(t)
	r2.EXPECT().Run(mock.Anything, "/repo", "git", "status", "--porcelain").Return(ok("\n"), nil).Once()

	clean, err := gitadapter.New(r2).Dirty(context.Background(), "/repo")
	require.NoError(t, err)
	require.False(t, clean)
}

func TestAddAndCommit(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "add", ".").Return(ok(""), nil).Once()
	r.EXPECT().Run(mock.Anything, "/repo", "git", "commit", "-m", "ci: x").Return(ok(""), nil).Once()

	g := gitadapter.New(r)
	require.NoError(t, g.Add(context.Background(), "/repo", "."))
	require.NoError(t, g.Commit(context.Background(), "/repo", "ci: x"))
}

func TestANonZeroGitExitCarriesStderr(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "rev-parse", "HEAD").
		Return(execadapter.Result{ExitCode: 128, Stderr: "fatal: no commits yet\n"}, nil).Once()

	_, err := gitadapter.New(r).HeadSHA(context.Background(), "/repo")
	require.Error(t, err)
	require.Contains(t, err.Error(), "reading HEAD of /repo")
	require.Contains(t, err.Error(), "fatal: no commits yet")
}

func TestABrokenRunnerIsWrappedWithWhatWasAttempted(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, mock.Anything, "git", "init", "-b", "main").
		Return(execadapter.Result{}, errBoom).Once()

	err := gitadapter.New(r).Init(context.Background(), "/repo")
	require.ErrorIs(t, err, errBoom)
	require.Contains(t, err.Error(), "initialising /repo")
}

func TestACleanWorktreeHashesToNothing(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "status", "--porcelain").Return(ok("\n"), nil).Once()

	got, err := gitadapter.New(r).WorktreeHash(context.Background(), "/repo")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestADirtyWorktreeHashesItsContent(t *testing.T) {
	build := func(status, diff string) string {
		r := execadaptermock.NewMockRunner(t)
		r.EXPECT().Run(mock.Anything, "/repo", "git", "status", "--porcelain").Return(ok(status), nil).Once()
		r.EXPECT().Run(mock.Anything, "/repo", "git", "diff", "HEAD").Return(ok(diff), nil).Once()

		got, err := gitadapter.New(r).WorktreeHash(context.Background(), "/repo")
		require.NoError(t, err)
		require.Len(t, got, 12)

		return got
	}

	one := build(" M a.go\n", "-old\n+new\n")
	same := build(" M a.go\n", "-old\n+new\n")
	other := build(" M a.go\n", "-old\n+newer\n")

	require.Equal(t, one, same, "the same edit gives the same revision")
	require.NotEqual(t, one, other, "a different edit gives a different revision")
}

func TestWorktreeHashReportsBothFailures(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "status", "--porcelain").
		Return(execadapter.Result{ExitCode: 128, Stderr: "fatal\n"}, nil).Once()

	_, err := gitadapter.New(r).WorktreeHash(context.Background(), "/repo")
	require.Error(t, err)

	r2 := execadaptermock.NewMockRunner(t)
	r2.EXPECT().Run(mock.Anything, "/repo", "git", "status", "--porcelain").Return(ok(" M a.go\n"), nil).Once()
	r2.EXPECT().Run(mock.Anything, "/repo", "git", "diff", "HEAD").
		Return(execadapter.Result{ExitCode: 128, Stderr: "fatal\n"}, nil).Once()

	_, err = gitadapter.New(r2).WorktreeHash(context.Background(), "/repo")
	require.Error(t, err)
	require.Contains(t, err.Error(), "reading uncommitted changes in /repo")
}

var errFake = errors.New("boom")

func TestCheckoutMovesARepoOntoOneCommit(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().Run(mock.Anything, "/w/a", "git", "status", "--porcelain").
		Return(execadapter.Result{}, nil).Once()
	runner.EXPECT().Run(mock.Anything, "/w/a", "git", "checkout", "--detach", "aaa").
		Return(execadapter.Result{}, nil).Once()

	require.NoError(t, gitadapter.New(runner).Checkout(t.Context(), "/w/a", "aaa"))
}

func TestCheckoutRefusesToDestroyUncommittedWork(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().Run(mock.Anything, "/w/a", "git", "status", "--porcelain").
		Return(execadapter.Result{Stdout: " M main.go\n"}, nil).Once()

	err := gitadapter.New(runner).Checkout(t.Context(), "/w/a", "aaa")
	require.ErrorContains(t, err, "uncommitted changes")
}

func TestCheckoutReportsAFailureToReadTheWorktree(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().Run(mock.Anything, mock.Anything, "git", "status", "--porcelain").
		Return(execadapter.Result{}, errFake).Once()

	require.Error(t, gitadapter.New(runner).Checkout(t.Context(), "/w/a", "aaa"))
}

func TestFetch(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().Run(mock.Anything, "/w/a", "git", "fetch", "--all", "--tags", "--quiet").
		Return(execadapter.Result{}, nil).Once()

	require.NoError(t, gitadapter.New(runner).Fetch(t.Context(), "/w/a"))
}

func TestLatestTagTakesTheHighestSemver(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().Run(mock.Anything, "/w/a", "git", "tag", "--sort=-v:refname").
		Return(execadapter.Result{Stdout: "v0.3.0\nv0.2.0\nv0.1.0\n"}, nil).Once()

	got, err := gitadapter.New(runner).LatestTag(t.Context(), "/w/a")
	require.NoError(t, err)
	require.Equal(t, "v0.3.0", got)
}

func TestARepoWithNoTagAnswersEmpty(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().Run(mock.Anything, mock.Anything, "git", "tag", "--sort=-v:refname").
		Return(execadapter.Result{Stdout: "\n"}, nil).Once()

	got, err := gitadapter.New(runner).LatestTag(t.Context(), "/w/a")
	require.NoError(t, err)
	require.Empty(t, got, "most members never carry a tag")
}

func TestLatestTagReportsAFailure(t *testing.T) {
	t.Parallel()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().Run(mock.Anything, mock.Anything, "git", "tag", "--sort=-v:refname").
		Return(execadapter.Result{}, errFake).Once()

	_, err := gitadapter.New(runner).LatestTag(t.Context(), "/w/a")
	require.Error(t, err)
}

func TestShowReadsAFileAtARevision(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "show", "v0.1.0:index/go/p/1.json").
		Return(ok(`{"current":"v1"}`), nil).Once()

	raw, found, err := gitadapter.New(r).Show(context.Background(), "/repo", "v0.1.0", "index/go/p/1.json")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, `{"current":"v1"}`, raw)
}

func TestShowAnswersFoundFalseForAPathTheRevisionDoesNotCarry(t *testing.T) {
	for _, stderr := range []string{
		"fatal: path 'x' does not exist in 'v0.1.0'",
		"fatal: path 'x' exists on disk, but not in 'v0.1.0'",
	} {
		r := execadaptermock.NewMockRunner(t)
		r.EXPECT().Run(mock.Anything, "/repo", "git", "show", "v0.1.0:x").
			Return(execadapter.Result{ExitCode: 128, Stderr: stderr}, nil).Once()

		_, found, err := gitadapter.New(r).Show(context.Background(), "/repo", "v0.1.0", "x")
		require.NoError(t, err)
		require.False(t, found)
	}
}

func TestShowReportsAnyOtherGitFailure(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "show", "v0.1.0:x").
		Return(execadapter.Result{ExitCode: 128, Stderr: "fatal: bad revision"}, nil).Once()

	_, _, err := gitadapter.New(r).Show(context.Background(), "/repo", "v0.1.0", "x")
	require.ErrorContains(t, err, "bad revision")

	r2 := execadaptermock.NewMockRunner(t)
	r2.EXPECT().Run(mock.Anything, "/repo", "git", "show", "v0.1.0:x").
		Return(execadapter.Result{}, errBoom).Once()

	_, _, err = gitadapter.New(r2).Show(context.Background(), "/repo", "v0.1.0", "x")
	require.ErrorIs(t, err, errBoom)
}

func TestLsTreeListsBasenames(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "ls-tree", "--name-only", "v0.1.0", "index/go/p/").
		Return(ok("index/go/p/1.json\nindex/go/p/2.json\n\n"), nil).Once()

	names, err := gitadapter.New(r).LsTree(context.Background(), "/repo", "v0.1.0", "index/go/p")
	require.NoError(t, err)
	require.Equal(t, []string{"1.json", "2.json"}, names)
}

func TestLsTreeReportsAFailure(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "ls-tree", "--name-only", "v0.1.0", "x/").
		Return(execadapter.Result{ExitCode: 128, Stderr: "fatal: not a tree"}, nil).Once()

	_, err := gitadapter.New(r).LsTree(context.Background(), "/repo", "v0.1.0", "x")
	require.ErrorContains(t, err, "not a tree")

	r2 := execadaptermock.NewMockRunner(t)
	r2.EXPECT().Run(mock.Anything, "/repo", "git", "ls-tree", "--name-only", "v0.1.0", "x/").
		Return(execadapter.Result{}, errBoom).Once()

	_, err = gitadapter.New(r2).LsTree(context.Background(), "/repo", "v0.1.0", "x")
	require.ErrorIs(t, err, errBoom)
}

func TestAheadBehindCountsBothSides(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "rev-list", "--left-right", "--count", "HEAD...origin/main").
		Return(ok("2\t5\n"), nil).Once()

	ahead, behind, err := gitadapter.New(r).AheadBehind(context.Background(), "/repo", "origin/main")
	require.NoError(t, err)
	require.Equal(t, 2, ahead)
	require.Equal(t, 5, behind)
}

func TestAheadBehindReportsFailures(t *testing.T) {
	r := execadaptermock.NewMockRunner(t)
	r.EXPECT().Run(mock.Anything, "/repo", "git", "rev-list", "--left-right", "--count", "HEAD...origin/main").
		Return(execadapter.Result{ExitCode: 128, Stderr: "fatal: unknown ref"}, nil).Once()

	_, _, err := gitadapter.New(r).AheadBehind(context.Background(), "/repo", "origin/main")
	require.ErrorContains(t, err, "unknown ref")

	r2 := execadaptermock.NewMockRunner(t)
	r2.EXPECT().Run(mock.Anything, "/repo", "git", "rev-list", "--left-right", "--count", "HEAD...origin/main").
		Return(ok("nonsense"), nil).Once()

	_, _, err = gitadapter.New(r2).AheadBehind(context.Background(), "/repo", "origin/main")
	require.ErrorContains(t, err, "unexpected output")

	r3 := execadaptermock.NewMockRunner(t)
	r3.EXPECT().Run(mock.Anything, "/repo", "git", "rev-list", "--left-right", "--count", "HEAD...origin/main").
		Return(ok("x y"), nil).Once()

	_, _, err = gitadapter.New(r3).AheadBehind(context.Background(), "/repo", "origin/main")
	require.Error(t, err)
}
