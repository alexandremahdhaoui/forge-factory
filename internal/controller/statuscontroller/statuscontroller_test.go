package statuscontroller_test

import (
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/statuscontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/fsadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/gitadaptermock"
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
`

func parse(t *testing.T, raw string) config.Factory {
	t.Helper()

	f, err := config.Parse([]byte(raw))
	require.NoError(t, err)

	return f
}

func TestStatusReportsACleanWorkspace(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/golden-go").Return(true, nil).Once()
	fs.EXPECT().IsDir("/w/golden-go").Return(true, nil).Once()
	fs.EXPECT().List("/w").Return([]string{"golden-go"}, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(true, nil).Once()
	git.EXPECT().Dirty(mock.Anything, "/w/golden-go").Return(false, nil).Once()
	git.EXPECT().HeadSHA(mock.Anything, "/w/golden-go").Return("aaa111", nil).Once()

	report, err := statuscontroller.New(fs, git).Status(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)

	assert.True(t, report.Agrees())
	require.Len(t, report.Repos, 1)
	assert.Equal(t, "aaa111", report.Repos[0].Head)
	assert.True(t, report.Repos[0].Cloned)
	assert.False(t, report.Repos[0].Dirty)
}

func TestStatusReportsAMemberThatIsNotThere(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/golden-go").Return(false, nil).Once()
	fs.EXPECT().List("/w").Return([]string{}, nil).Once()

	report, err := statuscontroller.New(fs, gitadaptermock.NewMockGit(t)).
		Status(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)

	assert.False(t, report.Agrees())
	assert.False(t, report.Repos[0].Present)
}

func TestStatusIgnoresAFileSittingBesideTheMembers(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/golden-go").Return(false, nil).Once()
	fs.EXPECT().List("/w").Return([]string{"forge-factory.yaml"}, nil).Once()
	fs.EXPECT().IsDir("/w/forge-factory.yaml").Return(false, nil).Once()

	report, err := statuscontroller.New(fs, gitadaptermock.NewMockGit(t)).
		Status(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)
	assert.Empty(t, report.Unknown, "git refuses to run inside a file")
}

func TestStatusReportsAFailureInspectingAPath(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/golden-go").Return(true, nil).Once()
	fs.EXPECT().IsDir("/w/golden-go").Return(false, assert.AnError).Once()

	_, err := statuscontroller.New(fs, gitadaptermock.NewMockGit(t)).
		Status(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}

func TestStatusReportsADirectoryThatIsNotARepo(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/golden-go").Return(true, nil).Once()
	fs.EXPECT().IsDir("/w/golden-go").Return(true, nil).Once()
	fs.EXPECT().List("/w").Return([]string{"golden-go"}, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(false, nil).Once()

	report, err := statuscontroller.New(fs, git).Status(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)

	assert.False(t, report.Agrees())
	assert.True(t, report.Repos[0].Present)
	assert.False(t, report.Repos[0].Cloned)
}

func TestStatusReportsARepoTheFactoryDoesNotDeclare(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/golden-go").Return(true, nil).Once()
	fs.EXPECT().IsDir("/w/golden-go").Return(true, nil).Once()
	fs.EXPECT().List("/w").Return([]string{"golden-go", "stray", "notes", "forge-factory.yaml"}, nil).Once()
	fs.EXPECT().IsDir("/w/stray").Return(true, nil).Once()
	fs.EXPECT().IsDir("/w/notes").Return(true, nil).Once()
	fs.EXPECT().IsDir("/w/forge-factory.yaml").Return(false, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(true, nil).Once()
	git.EXPECT().IsRepo(mock.Anything, "/w/stray").Return(true, nil).Once()
	git.EXPECT().IsRepo(mock.Anything, "/w/notes").Return(false, nil).Once()
	git.EXPECT().Dirty(mock.Anything, mock.Anything).Return(true, nil).Once()
	git.EXPECT().HeadSHA(mock.Anything, mock.Anything).Return("", assert.AnError).Once()

	report, err := statuscontroller.New(fs, git).Status(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)

	assert.False(t, report.Agrees())
	assert.Equal(t, []string{"stray"}, report.Unknown)
	assert.True(t, report.Repos[0].Dirty)
	assert.Empty(t, report.Repos[0].Head, "a head that cannot be read is left blank")
}

func TestStatusFailsWhenTheDiskCannotBeRead(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists(mock.Anything).Return(false, assert.AnError).Once()

	_, err := statuscontroller.New(fs, gitadaptermock.NewMockGit(t)).
		Status(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}

func TestStatusFailsWhenListingTheRootFails(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists(mock.Anything).Return(false, nil).Once()
	fs.EXPECT().List("/w").Return(nil, assert.AnError).Once()

	_, err := statuscontroller.New(fs, gitadaptermock.NewMockGit(t)).
		Status(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}

func TestStatusFailsWhenAMemberCannotBeInspected(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists(mock.Anything).Return(true, nil).Once()
	fs.EXPECT().IsDir(mock.Anything).Return(true, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(false, assert.AnError).Once()

	_, err := statuscontroller.New(fs, git).Status(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}

func TestStatusFailsWhenDirtyCannotBeAnswered(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists(mock.Anything).Return(true, nil).Once()
	fs.EXPECT().IsDir(mock.Anything).Return(true, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, mock.Anything).Return(true, nil).Once()
	git.EXPECT().Dirty(mock.Anything, mock.Anything).Return(false, assert.AnError).Once()

	_, err := statuscontroller.New(fs, git).Status(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}

func TestStatusFailsWhenAnUnknownDirectoryCannotBeInspected(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists(mock.Anything).Return(false, nil).Once()
	fs.EXPECT().List("/w").Return([]string{"stray"}, nil).Once()
	fs.EXPECT().IsDir("/w/stray").Return(true, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/stray").Return(false, assert.AnError).Once()

	_, err := statuscontroller.New(fs, git).Status(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}
