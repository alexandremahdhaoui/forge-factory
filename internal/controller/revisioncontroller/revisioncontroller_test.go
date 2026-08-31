package revisioncontroller_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/revisioncontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/engineadaptermock"
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
  - name: golden-rust
    url: u
    languages: [rust]
engines:
  - alias: go
    engine: forge://example.com/lang-go
  - alias: rust
    engine: forge://example.com/lang-rust
state:
  engine: forge://example.com/ci-state-git
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

// lists pins the one list call the restore pass makes, answering the given
// keys relative to the revision's prefix.
func lists(t *testing.T, caller *engineadaptermock.MockCaller, keys ...string) {
	t.Helper()

	caller.EXPECT().Call(mock.Anything, mock.Anything, "list", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			raw, err := json.Marshal(map[string]any{"keys": keys})
			require.NoError(t, err)

			return json.Unmarshal(raw, out)
		}).Once()
}

// lockPayload builds a stored dependency-lock record whose sha256 the test
// computes for itself, so the controller's verification is checked against an
// independent hash rather than its own.
func lockPayload(t *testing.T, revision, path, content string) map[string]any {
	t.Helper()

	sum := sha256.Sum256([]byte(content))

	payload, err := json.Marshal(map[string]any{
		"revision": revision,
		"path":     path,
		"sha256":   hex.EncodeToString(sum[:]),
		"lockfile": content,
	})
	require.NoError(t, err)

	return map[string]any{"found": true, "payload": string(payload)}
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

	caller.EXPECT().Call(mock.Anything, "forge://example.com/ci-state-git", "get", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &seen))

			payload, err := json.Marshal(found(map[string]string{"golden-go": "aaa"}, []string{"golden-rust"}))
			require.NoError(t, err)

			return json.Unmarshal(payload, out)
		}).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

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

	c := revisioncontroller.New(engineadaptermock.NewMockCaller(t), fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

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

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	rev, err := c.Get(t.Context(), f, "abc")
	require.NoError(t, err)

	assert.Equal(t, map[string]any{}, seen["spec"], "a nil map serialises to null and engines reject it")
	assert.Equal(t, "abc", rev.ID, "the asked for id stands in when the payload carries none")
}

func TestGetReportsARevisionTheEngineDoesNotHold(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, map[string]any{"found": false})

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err := c.Get(t.Context(), parse(t, factory), "nope")
	require.ErrorIs(t, err, revisioncontroller.ErrNotFound)
}

func TestGetFailsWhenTheEngineFails(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err := c.Get(t.Context(), parse(t, factory), "abc")
	require.ErrorIs(t, err, assert.AnError)
}

func TestGetReportsAPayloadThatIsNotARevision(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, map[string]any{"found": true, "payload": "not json at all"})

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err := c.Get(t.Context(), parse(t, factory), "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a revision")
}

func TestGetTakesTheAskedForIDWhenThePayloadIsEmpty(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, map[string]any{"found": true})

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	rev, err := c.Get(t.Context(), parse(t, factory), "abc")
	require.NoError(t, err)
	assert.Equal(t, "abc", rev.ID)
	assert.Empty(t, rev.Repos)
}

func TestGetSortsTheDirtyList(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(nil, []string{"golden-rust", "golden-go"}))

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	rev, err := c.Get(t.Context(), parse(t, factory), "abc")
	require.NoError(t, err)
	assert.Equal(t, []string{"golden-go", "golden-rust"}, rev.Dirty)
}

func TestCheckoutPutsEveryNamedMemberOnItsSHA(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa", "golden-rust": "bbb"}, nil))
	lists(t, caller)

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(true, nil).Once()
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-rust").Return(true, nil).Once()
	git.EXPECT().Checkout(mock.Anything, "/w/golden-go", "aaa").Return(nil).Once()
	git.EXPECT().Checkout(mock.Anything, "/w/golden-rust", "bbb").Return(nil).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), git)

	result, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.NoError(t, err)
	assert.Equal(t, "abc123", result.Revision)
	assert.Equal(t, map[string]string{"golden-go": "aaa", "golden-rust": "bbb"}, result.Repos)
}

func TestCheckoutLeavesAMemberTheRevisionDoesNotName(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa"}, nil))
	lists(t, caller)

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(true, nil).Once()
	git.EXPECT().Checkout(mock.Anything, "/w/golden-go", "aaa").Return(nil).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), git)

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

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), git)

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

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), git)

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, assert.AnError)
}

func TestCheckoutStopsWhenTheRepoCheckFails(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa"}, nil))

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, mock.Anything).Return(false, assert.AnError).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), git)

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, assert.AnError)
}

func TestCheckoutRestoresTheRevisionsDependencyLocks(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(map[string]string{"golden-go": "aaa"}, nil))

	var listSeen map[string]any

	caller.EXPECT().Call(mock.Anything, mock.Anything, "list", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &listSeen))

			payload, err := json.Marshal(map[string]any{
				"keys": []string{"golden-rust/Cargo.lock", "golden-go/go.sum"},
			})
			require.NoError(t, err)

			return json.Unmarshal(payload, out)
		}).Once()

	var getKeys []string

	caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			var seen map[string]any

			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &seen))

			key, _ := seen["key"].(string)
			getKeys = append(getKeys, key)

			var record map[string]any

			switch key {
			case "abc123/golden-go/go.sum":
				record = lockPayload(t, "abc123", "golden-go/go.sum", "sum lines\n")
			case "abc123/golden-rust/Cargo.lock":
				record = lockPayload(t, "abc123", "golden-rust/Cargo.lock", "[[package]]\n")
			default:
				t.Fatalf("unexpected get key %q", key)
			}

			payload, err := json.Marshal(record)
			require.NoError(t, err)

			return json.Unmarshal(payload, out)
		}).Times(2)

	git := gitadaptermock.NewMockGit(t)
	git.EXPECT().IsRepo(mock.Anything, "/w/golden-go").Return(true, nil).Once()
	git.EXPECT().Checkout(mock.Anything, "/w/golden-go", "aaa").Return(nil).Once()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().MkdirAll("/w/golden-go").Return(nil).Once()
	fs.EXPECT().MkdirAll("/w/golden-rust").Return(nil).Once()
	fs.EXPECT().WriteFile("/w/golden-go/go.sum", []byte("sum lines\n")).Return(nil).Once()
	fs.EXPECT().WriteFile("/w/golden-rust/Cargo.lock", []byte("[[package]]\n")).Return(nil).Once()

	c := revisioncontroller.New(caller, fs, git)

	result, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.NoError(t, err)

	assert.Equal(t, []string{"golden-go/go.sum", "golden-rust/Cargo.lock"}, result.Locks,
		"the restored paths are reported sorted")
	assert.Equal(t, []string{"abc123/golden-go/go.sum", "abc123/golden-rust/Cargo.lock"}, getKeys,
		"records are fetched under the revision's prefix, in sorted order")

	// The kind list is per-request caller configuration: checkout must name
	// the dependency-lock kind itself, or a store whose pipeline never
	// declared it refuses the list instead of answering empty.
	spec, ok := listSeen["spec"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, spec["kinds"], "dependency-lock")
	assert.Equal(t, "../golden-state", spec["path"], "the factory's own spec keys ride along")
}

func TestCheckoutDoesNotDuplicateADeclaredLockKind(t *testing.T) {
	t.Parallel()

	withKinds := factory + `    kinds: [dependency-lock]
`

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(nil, nil))

	var listSeen map[string]any

	caller.EXPECT().Call(mock.Anything, mock.Anything, "list", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &listSeen))

			return json.Unmarshal([]byte(`{"keys":[]}`), out)
		}).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	result, err := c.Checkout(t.Context(), parse(t, withKinds), "/w", "abc123")
	require.NoError(t, err)
	assert.Empty(t, result.Locks)

	spec, ok := listSeen["spec"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"dependency-lock"}, spec["kinds"],
		"a kind the pipeline already declares is not named twice")
}

func TestCheckoutRefusesALockThatDoesNotHashToItsRecord(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(nil, nil))
	lists(t, caller, "golden-go/go.sum")

	record, err := json.Marshal(map[string]any{
		"revision": "abc123",
		"path":     "golden-go/go.sum",
		"sha256":   "deadbeef",
		"lockfile": "sum lines\n",
	})
	require.NoError(t, err)

	caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			payload, err := json.Marshal(map[string]any{"found": true, "payload": string(record)})
			require.NoError(t, err)

			return json.Unmarshal(payload, out)
		}).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err = c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not hash to the recorded sha256")
}

func TestCheckoutRefusesALockPathThatEscapesTheWorkspace(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"../evil", "/etc/passwd", "a/../../evil"} {
		caller := engineadaptermock.NewMockCaller(t)
		answers(t, caller, found(nil, nil))
		lists(t, caller, "whatever")

		caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
				payload, err := json.Marshal(lockPayload(t, "abc123", path, "content"))
				require.NoError(t, err)

				return json.Unmarshal(payload, out)
			}).Once()

		c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

		_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
		require.Error(t, err, path)
		assert.Contains(t, err.Error(), "escapes the workspace", path)
	}
}

func TestCheckoutStopsWhenTheLockListFails(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(nil, nil))
	caller.EXPECT().Call(mock.Anything, mock.Anything, "list", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, assert.AnError)
}

func TestCheckoutStopsWhenAListedLockIsNotHeld(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(nil, nil))
	lists(t, caller, "golden-go/go.sum")
	answers(t, caller, map[string]any{"found": false})

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listed it and then did not hold it")
}

func TestCheckoutStopsWhenALockRecordIsNotALockRecord(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(nil, nil))
	lists(t, caller, "golden-go/go.sum")
	answers(t, caller, map[string]any{"found": true, "payload": "not json"})

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a lock record")
}

func TestCheckoutStopsWhenALockGetFails(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, found(nil, nil))
	lists(t, caller, "golden-go/go.sum")
	caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, assert.AnError)
}

func TestCheckoutStopsWhenALockCannotBeWritten(t *testing.T) {
	t.Parallel()

	answersLock := func(caller *engineadaptermock.MockCaller) {
		caller.EXPECT().Call(mock.Anything, mock.Anything, "get", mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
				payload, err := json.Marshal(lockPayload(t, "abc123", "golden-go/go.sum", "sum\n"))
				require.NoError(t, err)

				return json.Unmarshal(payload, out)
			}).Once()
	}

	t.Run("mkdir", func(t *testing.T) {
		t.Parallel()

		caller := engineadaptermock.NewMockCaller(t)
		answers(t, caller, found(nil, nil))
		lists(t, caller, "golden-go/go.sum")
		answersLock(caller)

		fs := fsadaptermock.NewMockFS(t)
		fs.EXPECT().MkdirAll("/w/golden-go").Return(assert.AnError).Once()

		c := revisioncontroller.New(caller, fs, gitadaptermock.NewMockGit(t))

		_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
		require.ErrorIs(t, err, assert.AnError)
	})

	t.Run("write", func(t *testing.T) {
		t.Parallel()

		caller := engineadaptermock.NewMockCaller(t)
		answers(t, caller, found(nil, nil))
		lists(t, caller, "golden-go/go.sum")
		answersLock(caller)

		fs := fsadaptermock.NewMockFS(t)
		fs.EXPECT().MkdirAll("/w/golden-go").Return(nil).Once()
		fs.EXPECT().WriteFile("/w/golden-go/go.sum", []byte("sum\n")).Return(assert.AnError).Once()

		c := revisioncontroller.New(caller, fs, gitadaptermock.NewMockGit(t))

		_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
		require.ErrorIs(t, err, assert.AnError)
	})
}

func TestCheckoutStopsWhenTheRevisionCannotBeRead(t *testing.T) {
	t.Parallel()

	caller := engineadaptermock.NewMockCaller(t)
	answers(t, caller, map[string]any{"found": false})

	c := revisioncontroller.New(caller, fsadaptermock.NewMockFS(t), gitadaptermock.NewMockGit(t))

	_, err := c.Checkout(t.Context(), parse(t, factory), "/w", "abc123")
	require.ErrorIs(t, err, revisioncontroller.ErrNotFound)
}
