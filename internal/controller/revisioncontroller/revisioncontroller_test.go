package revisioncontroller_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/revisioncontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/engineadaptermock"
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
  - name: golden-rust
    url: u
    languages: [rust]
engines:
  - alias: go
    engine: go://example.com/lang-go
  - alias: rust
    engine: go://example.com/lang-rust
state:
  engine: go://example.com/ci-state-git
  spec:
    path: ../golden-state
`

func parse(t *testing.T, raw string) config.Factory {
	t.Helper()

	f, err := config.Parse([]byte(raw))
	require.NoError(t, err)

	return f
}

func answers(t *testing.T, caller *engineadaptermock.MockCaller, payload any) {
	t.Helper()

	caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			raw, err := json.Marshal(payload)
			require.NoError(t, err)

			return json.Unmarshal(raw, out)
		}).Once()
}

// found builds what a conforming engine answers. The contract carries a payload
// as a JSON document in a string, not as an object.
func found(repos map[string]string, dirty []string) map[string]any {
	payload, err := json.Marshal(map[string]any{
		"id":    "abc123",
		"repos": repos,
		"dirty": dirty,
	})
	if err != nil {
		panic(err)
	}

	return map[string]any{"found": true, "payload": string(payload)}
}

func TestGetReadsARevisionThroughTheStateEngine(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)

	var seen map[string]any

	caller.EXPECT().Call(mock.Anything, "go://example.com/ci-state-git", "get", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &seen))

			payload, err := json.Marshal(found(map[string]string{"golden-go": "aaa"}, []string{"golden-rust"}))
			require.NoError(t, err)

			return json.Unmarshal(payload, out)
		}).Once()

	c := revisioncontroller.New(caller, gitadaptermock.NewMockGit(t))

	rev, err := c.Get(t.Context(), parse(t, factory), "abc123")
	require.NoError(t, err)

	assert.Equal(t, "revision", seen["kind"])
	assert.Equal(t, "abc123", seen["key"])
	assert.Equal(t, map[string]any{"path": "../golden-state"}, seen["spec"])
	assert.Equal(t, "abc123", rev.ID)
	assert.Equal(t, map[string]string{"golden-go": "aaa"}, rev.Repos)
	assert.Equal(t, []string{"golden-rust"}, rev.Dirty)
}

func TestGetRefusesAFactoryWithNoStateEngine(t *testing.T) {
	t.Parallel()

	f := parse(t, factory)
	f.State = nil

	c := revisioncontroller.New(engineadaptermock.NewMockCaller(t), gitadaptermock.NewMockGit(t))

	_, err := c.Get(t.Context(), f, "abc")
	require.ErrorIs(t, err, revisioncontroller.ErrNoState)
}

func TestGetSendsAnEmptySpecRatherThanNull(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)

	var seen map[string]any

	caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, _ := json.Marshal(in)
			require.NoError(t, json.Unmarshal(raw, &seen))

			return json.Unmarshal([]byte(`{"found":true}`), out)
		}).Once()

	f := parse(t, factory)
	f.State.Spec = nil

	c := revisioncontroller.New(caller, gitadaptermock.NewMockGit(t))

	rev, err := c.Get(t.Context(), f, "abc")
	require.NoError(t, err)

	assert.Equal(t, map[string]any{}, seen["spec"], "a nil map serialises to null and engines reject it")
	assert.Equal(t, "abc", rev.ID, "the asked for id stands in when the payload carries none")
}

func TestGetReportsARevisionTheEngineDoesNotHold(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, map[string]any{"found": false})

	c := revisioncontroller.New(caller, gitadaptermock.NewMockGit(t))

	_, err := c.Get(t.Context(), parse(t, factory), "nope")
	require.ErrorIs(t, err, revisioncontroller.ErrNotFound)
}

func TestGetFailsWhenTheEngineFails(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	c := revisioncontroller.New(caller, gitadaptermock.NewMockGit(t))

	_, err := c.Get(t.Context(), parse(t, factory), "abc")
	require.ErrorIs(t, err, assert.AnError)
}

func TestGetReportsAPayloadThatIsNotARevision(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, map[string]any{"found": true, "payload": "not json at all"})

	c := revisioncontroller.New(caller, gitadaptermock.NewMockGit(t))

	_, err := c.Get(t.Context(), parse(t, factory), "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a revision")
}

func TestGetTakesTheAskedForIDWhenThePayloadIsEmpty(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, map[string]any{"found": true})

	c := revisioncontroller.New(caller, gitadaptermock.NewMockGit(t))

	rev, err := c.Get(t.Context(), parse(t, factory), "abc")
	require.NoError(t, err)
	assert.Equal(t, "abc", rev.ID)
	assert.Empty(t, rev.Repos)
}

func TestGetSortsTheDirtyList(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(nil, []string{"golden-rust", "golden-go"}))

	c := revisioncontroller.New(caller, gitadaptermock.NewMockGit(t))

	rev, err := c.Get(t.Context(), parse(t, factory), "abc")
	require.NoError(t, err)
	assert.Equal(t, []string{"golden-go", "golden-rust"}, rev.Dirty)
}

func TestCheckoutPutsEveryNamedMemberOnItsSHA(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa", "golden-rust": "bbb"}, nil))

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(true, nil).Once()
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-rust").Return(true, nil).Once()
	git.EXPECT().Checkout(mock.Anything, "/w/golden-go", "aaa").Return(nil).Once()
	git.EXPECT().Checkout(mock.Anything, "/w/golden-rust", "bbb").Return(nil).Once()

	c := revisioncontroller.New(caller, git)

	result, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.NoError(t, err)
	assert.Equal(t, "abc123", result.Revision)
	assert.Equal(t, map[string]string{"golden-go": "aaa", "golden-rust": "bbb"}, result.Repos)
}

func TestCheckoutLeavesAMemberTheRevisionDoesNotName(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa"}, nil))

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(true, nil).Once()
	git.EXPECT().Checkout(mock.Anything, "/w/golden-go", "aaa").Return(nil).Once()

	c := revisioncontroller.New(caller, git)

	result, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.NoError(t, err)
	assert.Len(t, result.Repos, 1)
}

func TestCheckoutRefusesAMemberThatIsNotClonedYet(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa"}, nil))

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(false, nil).Once()

	c := revisioncontroller.New(caller, git)

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, revisioncontroller.ErrMissing)
}

func TestCheckoutStopsWhenGitRefuses(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa"}, nil))

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, mock.Anything).Return(true, nil).Once()
	git.EXPECT().Checkout(mock.Anything, mock.Anything, mock.Anything).Return(assert.AnError).Once()

	c := revisioncontroller.New(caller, git)

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, assert.AnError)
}

func TestCheckoutStopsWhenTheRepoCheckFails(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa"}, nil))

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, mock.Anything).Return(false, assert.AnError).Once()

	c := revisioncontroller.New(caller, git)

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, assert.AnError)
}

func TestCheckoutStopsWhenTheRevisionCannotBeRead(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, map[string]any{"found": false})

	c := revisioncontroller.New(caller, gitadaptermock.NewMockGit(t))

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, revisioncontroller.ErrNotFound)
}
