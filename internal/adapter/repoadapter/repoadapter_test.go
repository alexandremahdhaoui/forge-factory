package repoadapter_test

import (
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/repoadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/fsadaptermock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityReadsTheFactoryBlock(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/a/forge.yaml").Return(true, nil).Once()
	fs.EXPECT().ReadFile("/w/a/forge.yaml").Return([]byte(`name: a
build:
  - name: b
    src: .
factory:
  module: example.com/a
  goVersion: "1.26"
`), nil).Once()

	got, err := repoadapter.New(fs).Identity("/w/a")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"module": "example.com/a", "goVersion": "1.26"}, got)
}

func TestIdentityIsEmptyWhenTheRepoDeclaresNone(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/a/forge.yaml").Return(true, nil).Once()
	fs.EXPECT().ReadFile("/w/a/forge.yaml").Return([]byte("name: a\n"), nil).Once()

	got, err := repoadapter.New(fs).Identity("/w/a")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{}, got)
}

func TestIdentityIsEmptyWhenTheRepoHasNoForgeYAML(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/a/forge.yaml").Return(false, nil).Once()

	got, err := repoadapter.New(fs).Identity("/w/a")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestIdentityReportsAForgeYAMLThatDoesNotParse(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/a/forge.yaml").Return(true, nil).Once()
	fs.EXPECT().ReadFile("/w/a/forge.yaml").Return([]byte("factory: [not a map]\n"), nil).Once()

	_, err := repoadapter.New(fs).Identity("/w/a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the factory block")
}

func TestIdentityReportsADiskFailure(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().Exists("/w/a/forge.yaml").Return(false, assert.AnError).Once()

	_, err := repoadapter.New(fs).Identity("/w/a")
	require.ErrorIs(t, err, assert.AnError)

	fs2 := fsadaptermock.NewMockFS(t)
	fs2.EXPECT().Exists("/w/a/forge.yaml").Return(true, nil).Once()
	fs2.EXPECT().ReadFile("/w/a/forge.yaml").Return(nil, assert.AnError).Once()

	_, err = repoadapter.New(fs2).Identity("/w/a")
	require.ErrorIs(t, err, assert.AnError)
}
