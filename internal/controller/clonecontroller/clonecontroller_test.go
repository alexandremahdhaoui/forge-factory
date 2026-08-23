package clonecontroller_test

import (
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/clonecontroller"
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
    url: git@github.com:x/golden-go.git
    languages: [go]
  - name: golden-rust
    url: git@github.com:x/golden-rust.git
    languages: [rust]
engines:
  - alias: go
    engine: forge://example.com/lang-go
  - alias: rust
    engine: forge://example.com/lang-rust
`

func parse(t *testing.T, raw string) config.Factory {
	t.Helper()

	f, err := config.Parse([]byte(raw))
	require.NoError(t, err)

	return f
}

func TestCloneFetchesWhatIsMissing(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/golden-go").Return(false, nil).Once()
	fs.EXPECT().Exists("/w/golden-rust").Return(false, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().Clone(mock.Anything, "git@github.com:x/golden-go.git", "/w/golden-go").
		Return(nil).Once()
	git.EXPECT().Clone(mock.Anything, "git@github.com:x/golden-rust.git", "/w/golden-rust").
		Return(nil).Once()

	report, err := clonecontroller.New(fs, git).Clone(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)
	assert.Equal(t, []string{"golden-go", "golden-rust"}, report.Cloned)
	assert.Empty(t, report.Present)
}

func TestCloneLeavesAMemberThatIsAlreadyThere(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/golden-go").Return(true, nil).Once()
	fs.EXPECT().Exists("/w/golden-rust").Return(false, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().Clone(mock.Anything, mock.Anything, "/w/golden-rust").Return(nil).Once()

	report, err := clonecontroller.New(fs, git).Clone(t.Context(), parse(t, factory), "/w")
	require.NoError(t, err)
	assert.Equal(t, []string{"golden-rust"}, report.Cloned)
	assert.Equal(t, []string{"golden-go"}, report.Present,
		"a clone must never be the thing that discards local work")
}

func TestCloneStopsWhenGitFails(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists(mock.Anything).Return(false, nil).Once()

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().Clone(mock.Anything, mock.Anything, mock.Anything).Return(assert.AnError).Once()

	_, err := clonecontroller.New(fs, git).Clone(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "cloning golden-go")
}

func TestCloneReportsADiskFailure(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists(mock.Anything).Return(false, assert.AnError).Once()

	_, err := clonecontroller.New(fs, gitadaptermock.NewMockGit(t)).
		Clone(t.Context(), parse(t, factory), "/w")
	require.ErrorIs(t, err, assert.AnError)
}
