package synccontroller_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/engineadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/repoadaptermock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const goMod = "module example.com/g\n"

func recordDocument(t *testing.T, entries ...synccontroller.RenderedFile) []byte {
	t.Helper()

	raw, err := json.MarshalIndent(struct {
		Version int                           `json:"version"`
		Files   []synccontroller.RenderedFile `json:"files"`
	}{Version: 1, Files: entries}, "", "  ")
	require.NoError(t, err)

	return append(raw, '\n')
}

func renderingHarness(t *testing.T, content string) *harness {
	t.Helper()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
	h.answers("forge://example.com/lang-go", "render", map[string]any{"files": []map[string]any{
		{"path": "/w/golden-go/go.mod", "content": content},
	}})
	h.recordWrites()

	return h
}

func TestAFileThatMatchesItsLastRenderIsOverwrittenSilently(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = recordDocument(t, synccontroller.RenderedFile{
		Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte("an older render\n")),
	})
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(true, nil).Once()
	h.goMods["/w/golden-go/go.mod"] = "an older render\n"

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Equal(t, goMod, h.wrote["/w/golden-go/go.mod"])
	assert.Contains(t, report.Written, "/w/golden-go/go.mod")
}

func TestAFileThatWasNeverRenderedBeforeIsWrittenAndRecorded(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Equal(t, goMod, h.wrote["/w/golden-go/go.mod"])
	assert.Contains(t, h.wrote["/w/.forge/factory-generated.json"], synccontroller.Digest([]byte(goMod)))
}

func TestAFileRecordedWithNoDigestIsWrittenAndGainsOne(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = []byte(`{"version":1,"files":["golden-go/go.mod"]}` + "\n")

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Equal(t, goMod, h.wrote["/w/golden-go/go.mod"])
	assert.Contains(t, h.wrote["/w/.forge/factory-generated.json"], synccontroller.Digest([]byte(goMod)))
}

func TestAFileThatDiffersFromItsLastRenderIsNotTouched(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = recordDocument(t, synccontroller.RenderedFile{
		Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte("the last render\n")),
	})
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(true, nil).Once()
	h.goMods["/w/golden-go/go.mod"] = "a person edited this\n"

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, synccontroller.ErrDiffersFromLastRender)

	_, written := h.wrote["/w/golden-go/go.mod"]
	assert.False(t, written, "a refused render writes nothing")
}

func TestTheRefusalNamesTheFileTheReasonAndBothWaysForward(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = recordDocument(t, synccontroller.RenderedFile{
		Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte("the last render\n")),
	})
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(true, nil).Once()
	h.goMods["/w/golden-go/go.mod"] = "a person edited this\n"

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "golden-go/go.mod")
	assert.Contains(t, message, "not the one it last wrote")
	assert.Contains(t, message, "Keep the edit.")
	assert.Contains(t, message, "forge-factory take golden-go/go.mod")
	assert.Contains(t, message, "golden-go/go.mod.bak")
}

func TestTheRefusalNamesTheFileByItsWorkspaceRelativePath(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = recordDocument(t, synccontroller.RenderedFile{
		Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte("the last render\n")),
	})
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(true, nil).Once()
	h.goMods["/w/golden-go/go.mod"] = "a person edited this\n"

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "/w/golden-go/go.mod")
}

func TestARecordedFileThatVanishedIsWrittenBack(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = recordDocument(t, synccontroller.RenderedFile{
		Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte("the last render\n")),
	})
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(false, nil).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.Equal(t, goMod, h.wrote["/w/golden-go/go.mod"])
}

func TestAFileThatCannotBeStattedRefusesRatherThanRenderingOverIt(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = recordDocument(t, synccontroller.RenderedFile{
		Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte("the last render\n")),
	})
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(false, assert.AnError).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "inspecting /w/golden-go/go.mod before rendering over it")
}

func TestAFileThatCannotBeReadRefusesRatherThanRenderingOverIt(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = recordDocument(t, synccontroller.RenderedFile{
		Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte("the last render\n")),
	})
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(true, nil).Once()

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading /w/golden-go/go.mod before rendering over it")
}

func TestAMalformedRecordFailsByNameRatherThanRenderingOverEverything(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.envrcExists(t, true)
	h.repos.EXPECT().Identity("/w/golden-go").
		Return(map[string]string{"module": "example.com/g"}, nil).Once()
	h.recordWrites()
	h.record = []byte("{ not json")

	_, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the render record at /w/.forge/factory-generated.json")
}

func TestARecordEntryThatIsNeitherAStringNorAnObjectFailsByName(t *testing.T) {
	t.Parallel()

	var entry synccontroller.RenderedFile

	err := json.Unmarshal([]byte("42"), &entry)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading a rendered file entry")
}

func TestTakeCopiesTheFileToBakAndDropsItFromTheRecord(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t,
		synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"},
		synccontroller.RenderedFile{Path: "go.work", SHA256: "bbb"},
	)
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/Cargo.toml").Return([]byte("a person edited this\n"), nil).Once()
	h.fs.EXPECT().Exists("/w/Cargo.toml.bak").Return(false, nil).Once()
	h.recordWrites()

	report, err := h.c.Take("/w", "Cargo.toml")
	require.NoError(t, err)
	assert.Equal(t, "Cargo.toml", report.Path)
	assert.Equal(t, "Cargo.toml.bak", report.Backup)
	assert.Equal(t, "a person edited this\n", h.wrote["/w/Cargo.toml.bak"])
	assert.NotContains(t, h.wrote["/w/.forge/factory-generated.json"], "Cargo.toml")
	assert.Contains(t, h.wrote["/w/.forge/factory-generated.json"], "go.work")
}

func TestTakeAcceptsAnAbsolutePath(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/Cargo.toml").Return([]byte("edited\n"), nil).Once()
	h.fs.EXPECT().Exists("/w/Cargo.toml.bak").Return(false, nil).Once()
	h.recordWrites()

	report, err := h.c.Take("/w", "/w/Cargo.toml")
	require.NoError(t, err)
	assert.Equal(t, "Cargo.toml", report.Path)
}

func TestTakeNumbersTheBackupSoASecondEditIsNeverLost(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/Cargo.toml").Return([]byte("a second edit\n"), nil).Once()
	h.fs.EXPECT().Exists("/w/Cargo.toml.bak").Return(true, nil).Once()
	h.fs.EXPECT().Exists("/w/Cargo.toml.bak.2").Return(true, nil).Once()
	h.fs.EXPECT().Exists("/w/Cargo.toml.bak.3").Return(false, nil).Once()
	h.recordWrites()

	report, err := h.c.Take("/w", "Cargo.toml")
	require.NoError(t, err)
	assert.Equal(t, "Cargo.toml.bak.3", report.Backup)
	assert.Equal(t, "a second edit\n", h.wrote["/w/Cargo.toml.bak.3"])
}

func TestTakeRefusesAFileTheFactoryDoesNotRender(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})

	_, err := h.c.Take("/w", "NOTES.md")
	require.ErrorIs(t, err, synccontroller.ErrNotRendered)
	assert.Contains(t, err.Error(), "NOTES.md")
	assert.Contains(t, err.Error(), synccontroller.GeneratedManifestPath)
}

func TestTakeRefusesAFileThatIsNotOnDisk(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(false, nil).Once()

	_, err := h.c.Take("/w", "Cargo.toml")
	require.ErrorIs(t, err, synccontroller.ErrNothingToTake)
	assert.Contains(t, err.Error(), "forge-factory sync")
}

func TestTakeFailsByNameWhenTheRecordCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = []byte("{ not json")

	_, err := h.c.Take("/w", "Cargo.toml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the render record at")
}

func TestTakeFailsByNameWhenTheFileCannotBeStatted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(false, assert.AnError).Once()

	_, err := h.c.Take("/w", "Cargo.toml")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "inspecting Cargo.toml before taking it")
}

func TestTakeFailsByNameWhenTheFileCannotBeRead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/Cargo.toml").Return(nil, assert.AnError).Once()

	_, err := h.c.Take("/w", "Cargo.toml")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "reading Cargo.toml before taking it")
}

func TestTakeFailsByNameWhenNoBackupNameCanBeChecked(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/Cargo.toml").Return([]byte("edited\n"), nil).Once()
	h.fs.EXPECT().Exists("/w/Cargo.toml.bak").Return(false, assert.AnError).Once()

	_, err := h.c.Take("/w", "Cargo.toml")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "looking for a free backup name beside /w/Cargo.toml")
}

func TestTakeFailsByNameWhenTheBackupCannotBeWritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/Cargo.toml").Return([]byte("edited\n"), nil).Once()
	h.fs.EXPECT().Exists("/w/Cargo.toml.bak").Return(false, nil).Once()
	h.fs.EXPECT().WriteFile("/w/Cargo.toml.bak", mock.Anything).Return(assert.AnError).Once()

	_, err := h.c.Take("/w", "Cargo.toml")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "backing Cargo.toml up to Cargo.toml.bak")
}

func TestTakeFailsByNameWhenTheRecordCannotBeWritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.record = recordDocument(t, synccontroller.RenderedFile{Path: "Cargo.toml", SHA256: "aaa"})
	h.fs.EXPECT().Exists("/w/Cargo.toml").Return(true, nil).Once()
	h.fs.EXPECT().ReadFile("/w/Cargo.toml").Return([]byte("edited\n"), nil).Once()
	h.fs.EXPECT().Exists("/w/Cargo.toml.bak").Return(false, nil).Once()
	h.fs.EXPECT().WriteFile("/w/Cargo.toml.bak", mock.Anything).Return(nil).Once()
	h.recordWriteErr = assert.AnError

	_, err := h.c.Take("/w", "Cargo.toml")
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "recording the render of /w")
}

func TestARecordThatDidNotMoveIsNotRewritten(t *testing.T) {
	t.Parallel()

	h := renderingHarness(t, goMod)
	h.record = recordDocument(t, synccontroller.RenderedFile{
		Path: "golden-go/go.mod", SHA256: synccontroller.Digest([]byte(goMod)),
	})
	h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(true, nil).Once()
	h.goMods["/w/golden-go/go.mod"] = goMod

	report, err := h.c.Sync(t.Context(), parse(t, factory), "/w", "")
	require.NoError(t, err)
	assert.NotContains(t, report.Written, "/w/.forge/factory-generated.json")
}

const tidiedGoMod = "module example.com/g\n\nrequire sigs.k8s.io/yaml v1.6.0\n"

func synced(t *testing.T) (string, *synccontroller.Controller, *execadaptermock.MockRunner) {
	t.Helper()

	root := t.TempDir()
	member := filepath.Join(root, "golden-go")
	caller := engineadaptermock.NewMockCaller(t)
	repos := repoadaptermock.NewMockReader(t)
	runner := execadaptermock.NewMockRunner(t)

	repos.EXPECT().Identity(member).Return(map[string]string{"module": "example.com/g"}, nil).Maybe()
	caller.EXPECT().Call(mock.Anything, mock.Anything, synccontroller.ToolLanguage, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			return json.Unmarshal([]byte(`{"language":"go"}`), out)
		}).Maybe()
	caller.EXPECT().Call(mock.Anything, mock.Anything, synccontroller.ToolRender, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			raw, err := json.Marshal(map[string]any{
				"files":          []map[string]any{{"path": filepath.Join(member, "go.mod"), "content": goMod}},
				"dependencyLock": []map[string]any{{"dir": member, "command": "go", "args": []string{"mod", "tidy"}}},
			})
			if err != nil {
				return err
			}

			return json.Unmarshal(raw, out)
		}).Maybe()

	runner.EXPECT().RunEnv(mock.Anything, member, mock.Anything, "go", "mod", "tidy").
		RunAndReturn(func(context.Context, string, map[string]string, string, ...string) (execadapter.Result, error) {
			return execadapter.Result{}, os.WriteFile(filepath.Join(member, "go.mod"), []byte(tidiedGoMod), 0o600)
		})

	c := synccontroller.New(caller, fsadapter.New(), repos, runner, passthrough{})

	_, err := c.Sync(t.Context(), parse(t, factory), root, "")
	require.NoError(t, err)

	return root, c, runner
}

func TestASyncRightAfterASyncWhoseCommandRewroteGoModSucceeds(t *testing.T) {
	t.Parallel()

	root, c, _ := synced(t)

	record, err := synccontroller.ReadRecord(fsadapter.New(), root)
	require.NoError(t, err)
	assert.Equal(t, synccontroller.Digest([]byte(tidiedGoMod)), record["golden-go/go.mod"].SHA256,
		"the record holds go.mod as the command left it, not as the render wrote it")

	_, err = c.Sync(t.Context(), parse(t, factory), root, "")
	require.NoError(t, err)
}

func syncedAndLocked(t *testing.T) (string, *synccontroller.Controller) {
	t.Helper()

	root, c, _ := synced(t)
	member := filepath.Join(root, "golden-go")

	_, err := c.Lock(t.Context(), parse(t, factory), root, "")
	require.NoError(t, err)

	locked, err := os.ReadFile(filepath.Join(member, "go.mod"))
	require.NoError(t, err)
	require.Equal(t, tidiedGoMod, string(locked))

	return root, c
}

func TestASyncAfterALockAcceptsTheFileTheLockRewrote(t *testing.T) {
	t.Parallel()

	root, c := syncedAndLocked(t)

	_, err := c.Sync(t.Context(), parse(t, factory), root, "")
	require.NoError(t, err)
}

func TestALockFailsByNameWhenItCannotReadWhatItLeftOnDisk(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		exists  error
		message string
	}{
		"inspecting": {exists: assert.AnError, message: "inspecting /w/golden-go/go.mod as the lock left it"},
		"reading":    {message: "reading /w/golden-go/go.mod as the lock left it: no manifest yet"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.repos.EXPECT().Identity(mock.Anything).Return(map[string]string{}, nil).Once()
			h.answers("forge://example.com/lang-go", "language", map[string]any{"language": "go"})
			h.answers("forge://example.com/lang-go", "render", map[string]any{
				"files": []map[string]any{{"path": "/w/golden-go/go.mod", "content": goMod}},
			})
			h.fs.EXPECT().Exists("/w/golden-go/go.mod").Return(tc.exists == nil, tc.exists).Once()

			_, err := h.c.Lock(t.Context(), parse(t, factory), "/w", "")
			require.ErrorContains(t, err, tc.message)
		})
	}
}

func TestAHandEditMadeBeforeALockIsRefusedAndNoLockCommandRuns(t *testing.T) {
	t.Parallel()

	root, c, runner := synced(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "golden-go", "go.mod"),
		[]byte(goMod+"\nrequire example.com/by-hand v1.0.0\n"), 0o600))

	before := len(runner.Calls)

	_, err := c.Lock(t.Context(), parse(t, factory), root, "")
	require.ErrorIs(t, err, synccontroller.ErrDiffersFromLastRender)
	assert.Len(t, runner.Calls, before, "the refused lock adds no command to the ones the sync ran")
}

func TestAHandEditMadeAfterALockIsStillRefused(t *testing.T) {
	t.Parallel()

	root, c := syncedAndLocked(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "golden-go", "go.mod"),
		[]byte(tidiedGoMod+"\nrequire example.com/by-hand v1.0.0\n"), 0o600))

	_, err := c.Sync(t.Context(), parse(t, factory), root, "")
	require.ErrorIs(t, err, synccontroller.ErrDiffersFromLastRender)
}
