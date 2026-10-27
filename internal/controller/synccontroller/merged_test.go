package synccontroller_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/rendercontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/engineadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/repoadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/rendertypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const mergedFactory = `version: "1"
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
dependencies:
  go:
    sigs.k8s.io/yaml: v1.6.0
`

const memberGoModBeforeTheFactory = `module example.com/g

go 1.26

require sigs.k8s.io/yaml v1.5.0

require go.yaml.in/yaml/v4 v4.0.0 // indirect
`

const renderedCargoToml = "[workspace]\nmembers = [\"golden-rust\"]\n"

type mergedWorkspace struct {
	root   string
	c      *synccontroller.Controller
	mu     sync.Mutex
	tidyTo func(string) string
}

func (w *mergedWorkspace) goMod() string {
	return filepath.Join(w.root, "golden-go", "go.mod")
}

func (w *mergedWorkspace) read(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(raw)
}

func (w *mergedWorkspace) write(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, fsadapter.New().WriteFile(path, []byte(content)))
}

func (w *mergedWorkspace) replaceInGoMod(t *testing.T, old, next string) {
	t.Helper()

	before := w.read(t, w.goMod())
	require.Contains(t, before, old)
	w.write(t, w.goMod(), strings.Replace(before, old, next, 1))
}

func (w *mergedWorkspace) tidyRaises(module, from, to string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.tidyTo = func(content string) string {
		return strings.Replace(content, module+" "+from, module+" "+to, 1)
	}
}

func (w *mergedWorkspace) tidy() (execadapter.Result, error) {
	w.mu.Lock()
	transform := w.tidyTo
	w.mu.Unlock()

	if transform == nil {
		return execadapter.Result{}, nil
	}

	raw, err := os.ReadFile(w.goMod())
	if err != nil {
		return execadapter.Result{}, err
	}

	return execadapter.Result{}, os.WriteFile(w.goMod(), []byte(transform(string(raw))), 0o600)
}

func (w *mergedWorkspace) sync(t *testing.T) error {
	t.Helper()

	return w.syncDeclaring(t, "    sigs.k8s.io/yaml: v1.6.0\n")
}

func (w *mergedWorkspace) syncDeclaring(t *testing.T, goDependencies string) error {
	t.Helper()

	_, err := w.syncReport(t, goDependencies)

	return err
}

func (w *mergedWorkspace) syncReport(t *testing.T, goDependencies string) (synccontroller.Report, error) {
	t.Helper()

	factory := strings.Replace(mergedFactory, "    sigs.k8s.io/yaml: v1.6.0\n", goDependencies, 1)

	return w.c.Sync(t.Context(), parse(t, factory), w.root, "")
}

func (w *mergedWorkspace) lock(t *testing.T) error {
	t.Helper()

	_, err := w.c.Lock(t.Context(), parse(t, mergedFactory), w.root, "")

	return err
}

func (w *mergedWorkspace) recorded(t *testing.T, rel string) synccontroller.RenderedFile {
	t.Helper()

	record, err := synccontroller.ReadRecord(fsadapter.New(), w.root)
	require.NoError(t, err)

	return record[rel]
}

type renderRequest struct {
	Root  string `json:"root"`
	Repos []struct {
		Name      string            `json:"name"`
		Path      string            `json:"path"`
		Languages []string          `json:"languages"`
		Identity  map[string]string `json:"identity"`
	} `json:"repos"`
	Dependencies map[string]string `json:"dependencies"`
}

func renderGo(in any) (map[string]any, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	var req renderRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	input := rendertypes.Input{Root: req.Root, Dependencies: req.Dependencies}
	for _, r := range req.Repos {
		input.Repos = append(input.Repos, rendertypes.Repo{
			Name: r.Name, Path: r.Path, Languages: r.Languages, Identity: r.Identity,
		})
	}

	out, err := rendercontroller.NewGo(fsadapter.New()).Render(input)
	if err != nil {
		return nil, err
	}

	files := []map[string]any{}
	for _, f := range out.Files {
		files = append(files, map[string]any{"path": f.Path, "content": f.Content, "merged": f.Merged})
	}

	commands := []map[string]any{}
	for _, c := range out.DependencyLock {
		commands = append(commands, map[string]any{
			"dir": c.Dir, "command": c.Command, "args": c.Args, "env": c.Env, "optional": c.Optional,
		})
	}

	return map[string]any{"files": files, "dependencyLock": commands, "lockfiles": out.Lockfiles}, nil
}

func answer(out any, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}

	return json.Unmarshal(raw, out)
}

func newMergedWorkspace(t *testing.T) *mergedWorkspace {
	t.Helper()

	w := &mergedWorkspace{root: t.TempDir()}
	member := filepath.Join(w.root, "golden-go")

	require.NoError(t, os.MkdirAll(member, 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(w.root, "golden-rust"), 0o750))
	w.write(t, w.goMod(), memberGoModBeforeTheFactory)

	caller := engineadaptermock.NewMockCaller(t)
	repos := repoadaptermock.NewMockReader(t)
	runner := execadaptermock.NewMockRunner(t)

	repos.EXPECT().Identity(member).Return(map[string]string{"module": "example.com/g"}, nil).Maybe()
	repos.EXPECT().Identity(filepath.Join(w.root, "golden-rust")).Return(map[string]string{}, nil).Maybe()

	caller.EXPECT().Call(mock.Anything, "forge://example.com/lang-go", synccontroller.ToolLanguage, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			return answer(out, map[string]any{"language": "go"})
		}).Maybe()
	caller.EXPECT().Call(mock.Anything, "forge://example.com/lang-rust", synccontroller.ToolLanguage, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			return answer(out, map[string]any{"language": "rust"})
		}).Maybe()
	caller.EXPECT().Call(mock.Anything, "forge://example.com/lang-go", synccontroller.ToolRender, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			rendered, err := renderGo(in)
			if err != nil {
				return err
			}

			return answer(out, rendered)
		}).Maybe()
	caller.EXPECT().Call(mock.Anything, "forge://example.com/lang-rust", synccontroller.ToolRender, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			return answer(out, map[string]any{"files": []map[string]any{
				{"path": filepath.Join(w.root, "Cargo.toml"), "content": renderedCargoToml},
			}})
		}).Maybe()

	runner.EXPECT().RunEnv(mock.Anything, w.root, mock.Anything, "go", "work", "sync").
		Return(execadapter.Result{}, nil).Maybe()
	runner.EXPECT().RunEnv(mock.Anything, member, mock.Anything, "go", "mod", "tidy").
		RunAndReturn(func(context.Context, string, map[string]string, string, ...string) (execadapter.Result, error) {
			return w.tidy()
		}).Maybe()

	w.c = synccontroller.New(caller, fsadapter.New(), repos, runner, passthrough{})

	return w
}

func syncedMergedWorkspace(t *testing.T) *mergedWorkspace {
	t.Helper()

	w := newMergedWorkspace(t)
	require.NoError(t, w.sync(t))
	require.Contains(t, w.read(t, w.goMod()), "sigs.k8s.io/yaml v1.6.0")

	return w
}

const undeclaredEdit = "require go.yaml.in/yaml/v4 v4.0.0\n\nrequire example.com/by-hand v1.0.0\n"

func (w *mergedWorkspace) recordGoModAsAWholeFileDigest(t *testing.T) {
	t.Helper()

	w.write(t, filepath.Join(w.root, filepath.FromSlash(synccontroller.GeneratedManifestPath)),
		`{"version":1,"files":[{"path":"golden-go/go.mod","sha256":"`+synccontroller.Digest([]byte(w.read(t, w.goMod())))+`"}]}`+"\n")
}

var recordsBeforeAnUndeclaredEdit = map[string]func(*mergedWorkspace, *testing.T){
	"the record this sync wrote":                      func(*mergedWorkspace, *testing.T) {},
	"a whole file digest from before the merged mark": (*mergedWorkspace).recordGoModAsAWholeFileDigest,
}

func TestAMemberGoModEditedOutsideTheFactoryOnAnUndeclaredLineSyncsGreenAndKeepsTheEdit(t *testing.T) {
	t.Parallel()

	for name, recordBefore := range recordsBeforeAnUndeclaredEdit {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := syncedMergedWorkspace(t)
			recordBefore(w, t)
			w.replaceInGoMod(t, "require go.yaml.in/yaml/v4 v4.0.0 // indirect\n", undeclaredEdit)

			require.NoError(t, w.sync(t))
			assert.Contains(t, w.read(t, w.goMod()), undeclaredEdit)
		})
	}
}

func TestAMemberGoModEditedOutsideTheFactoryOnAnUndeclaredLineLocksGreen(t *testing.T) {
	t.Parallel()

	for name, recordBefore := range recordsBeforeAnUndeclaredEdit {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := syncedMergedWorkspace(t)
			recordBefore(w, t)
			w.replaceInGoMod(t, "require go.yaml.in/yaml/v4 v4.0.0 // indirect\n", undeclaredEdit)

			require.NoError(t, w.lock(t))
		})
	}
}

const raiseTheDeclaration = "Raise the declaration of each module above in forge-factory.yaml to the version the copy on disk requires. Then run forge-factory sync again."

func assertTheRefusalSaysWhatHappenedAndToRaiseTheDeclaration(t *testing.T, err error) {
	t.Helper()

	require.ErrorIs(t, err, synccontroller.ErrHandEdited)
	assert.Contains(t, err.Error(), "golden-go/go.mod")
	assert.Contains(t, err.Error(), "sigs.k8s.io/yaml: the record holds v1.6.0, the copy on disk requires v1.6.1")
	assert.Contains(t, err.Error(), raiseTheDeclaration)
	assert.NotContains(t, err.Error(), "by hand")
}

func TestAMemberGoModWhoseDeclaredVersionDiffersFromTheRecordAndTheDeclarationRefusesTheSyncAndSaysToRaiseTheDeclaration(t *testing.T) {
	t.Parallel()

	w := syncedMergedWorkspace(t)
	w.replaceInGoMod(t, "sigs.k8s.io/yaml v1.6.0", "sigs.k8s.io/yaml v1.6.1")

	assertTheRefusalSaysWhatHappenedAndToRaiseTheDeclaration(t, w.sync(t))
	assert.Contains(t, w.read(t, w.goMod()), "sigs.k8s.io/yaml v1.6.1", "a refused sync leaves the version on disk")
}

func TestAMemberGoModWhoseDeclaredVersionDiffersFromTheRecordAndTheDeclarationRefusesTheLockAndSaysToRaiseTheDeclaration(t *testing.T) {
	t.Parallel()

	w := syncedMergedWorkspace(t)
	w.replaceInGoMod(t, "sigs.k8s.io/yaml v1.6.0", "sigs.k8s.io/yaml v1.6.1")

	assertTheRefusalSaysWhatHappenedAndToRaiseTheDeclaration(t, w.lock(t))
}

func TestAModuleTheFactoryNoLongerDeclaresMovedOnDiskSyncsGreenAndKeepsTheVersionOnDisk(t *testing.T) {
	t.Parallel()

	w := newMergedWorkspace(t)
	require.NoError(t, w.syncDeclaring(t, "    sigs.k8s.io/yaml: v1.6.0\n    go.yaml.in/yaml/v4: v4.0.0\n"))
	require.Equal(t, map[string]string{"sigs.k8s.io/yaml": "v1.6.0", "go.yaml.in/yaml/v4": "v4.0.0"},
		w.recorded(t, "golden-go/go.mod").Versions)
	w.replaceInGoMod(t, "go.yaml.in/yaml/v4 v4.0.0", "go.yaml.in/yaml/v4 v4.0.1")

	require.NoError(t, w.sync(t))
	assert.Contains(t, w.read(t, w.goMod()), "go.yaml.in/yaml/v4 v4.0.1")
}

func TestAVersionOnDiskEqualToTheNewlyDeclaredVersionSyncsGreen(t *testing.T) {
	t.Parallel()

	w := syncedMergedWorkspace(t)
	w.replaceInGoMod(t, "sigs.k8s.io/yaml v1.6.0", "sigs.k8s.io/yaml v1.6.1")

	require.NoError(t, w.syncDeclaring(t, "    sigs.k8s.io/yaml: v1.6.1\n"))
	assert.Contains(t, w.read(t, w.goMod()), "sigs.k8s.io/yaml v1.6.1")
}

func TestASyncThatOverwritesADeclaredModuleReAddedWithNoRecordedVersionNamesBothVersionsInItsNotes(t *testing.T) {
	t.Parallel()

	declaring := "    sigs.k8s.io/yaml: v1.6.0\n    example.com/readded: v1.0.0\n"

	w := newMergedWorkspace(t)
	require.NoError(t, w.syncDeclaring(t, declaring))
	require.Equal(t, map[string]string{"sigs.k8s.io/yaml": "v1.6.0"}, w.recorded(t, "golden-go/go.mod").Versions)
	w.write(t, w.goMod(), w.read(t, w.goMod())+"\nrequire example.com/readded v0.9.0\n")

	report, err := w.syncReport(t, declaring)
	require.NoError(t, err)
	assert.Contains(t, w.read(t, w.goMod()), "example.com/readded v1.0.0")
	assert.Contains(t, report.Notes,
		"golden-go/go.mod: the sync moved example.com/readded from v0.9.0 on disk to v1.0.0, the version forge-factory declares")
}

func TestACargoTomlHandEditStillRefusesTheSyncAsAWholeFile(t *testing.T) {
	t.Parallel()

	w := syncedMergedWorkspace(t)
	cargo := filepath.Join(w.root, "Cargo.toml")
	w.write(t, cargo, renderedCargoToml+"\n[profile.release]\nlto = true\n")

	err := w.sync(t)
	require.ErrorIs(t, err, synccontroller.ErrHandEdited)
	assert.Contains(t, err.Error(), "Cargo.toml")
	assert.Contains(t, err.Error(), "not the one it last wrote")
	assert.Contains(t, w.read(t, cargo), "lto = true")
}

func TestARecordFromBeforeTheMergedMarkIsReadAndHoldsNoDeclaredVersionForTheGoMod(t *testing.T) {
	t.Parallel()

	for name, entry := range map[string]string{
		"a whole file digest": `{"path":"golden-go/go.mod","sha256":"` + synccontroller.Digest([]byte("an older render\n")) + `"}`,
		"a bare path":         `"golden-go/go.mod"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := newMergedWorkspace(t)
			w.write(t, filepath.Join(w.root, filepath.FromSlash(synccontroller.GeneratedManifestPath)),
				`{"version":1,"files":[`+entry+`]}`+"\n")

			require.NoError(t, w.sync(t))
			assert.Equal(t, synccontroller.RenderedFile{
				Path:     "golden-go/go.mod",
				Versions: map[string]string{"sigs.k8s.io/yaml": "v1.6.0"},
			}, w.recorded(t, "golden-go/go.mod"))
		})
	}
}

func TestASyncThatLeavesAMergedGoModUnchangedStillRecordsItsDeclaredVersions(t *testing.T) {
	t.Parallel()

	w := syncedMergedWorkspace(t)
	w.recordGoModAsAWholeFileDigest(t)

	require.NoError(t, w.sync(t))
	assert.Equal(t, map[string]string{"sigs.k8s.io/yaml": "v1.6.0"}, w.recorded(t, "golden-go/go.mod").Versions)
}

func TestASyncAfterALockWhoseTidyRaisedADeclaredVersionStaysGreen(t *testing.T) {
	t.Parallel()

	w := syncedMergedWorkspace(t)
	w.tidyRaises("sigs.k8s.io/yaml", "v1.6.0", "v1.7.0")

	require.NoError(t, w.lock(t))
	assert.Equal(t, map[string]string{"sigs.k8s.io/yaml": "v1.7.0"}, w.recorded(t, "golden-go/go.mod").Versions,
		"the lock records the declared version its tidy left on disk")

	require.NoError(t, w.sync(t))
}

const yamlPinIsStale = "golden-go/go.mod: the declared version of sigs.k8s.io/yaml is stale. " +
	"forge-factory.yaml declares v1.6.0 and the lock commands raised it to v1.7.0. " +
	"Set sigs.k8s.io/yaml to v1.7.0 in forge-factory.yaml."

func notesNaming(report synccontroller.Report, module string) []string {
	var out []string

	for _, note := range report.Notes {
		if strings.Contains(note, module) {
			out = append(out, note)
		}
	}

	return out
}

func TestASyncWhoseTidyRaisesADeclaredVersionNamesTheStalePinWithTheVersionToSetAndNoMove(t *testing.T) {
	t.Parallel()

	w := newMergedWorkspace(t)
	w.tidyRaises("sigs.k8s.io/yaml", "v1.6.0", "v1.7.0")

	report, err := w.syncReport(t, "    sigs.k8s.io/yaml: v1.6.0\n")
	require.NoError(t, err)
	assert.Equal(t, []string{yamlPinIsStale}, notesNaming(report, "sigs.k8s.io/yaml"))
}

func TestASyncAfterTheSyncThatNamedAStalePinNamesNothingAndNeverReportsAMoveDown(t *testing.T) {
	t.Parallel()

	w := newMergedWorkspace(t)
	w.tidyRaises("sigs.k8s.io/yaml", "v1.6.0", "v1.7.0")
	require.NoError(t, w.sync(t))

	report, err := w.syncReport(t, "    sigs.k8s.io/yaml: v1.6.0\n")
	require.NoError(t, err)
	assert.Empty(t, notesNaming(report, "sigs.k8s.io/yaml"))
	assert.Contains(t, w.read(t, w.goMod()), "sigs.k8s.io/yaml v1.7.0")
}

func TestANewlyDeclaredModuleBelowTheVersionOnDiskIsNamedAsAStalePinOnTheFirstSyncOnly(t *testing.T) {
	t.Parallel()

	declaring := "    sigs.k8s.io/yaml: v1.6.0\n    go.yaml.in/yaml/v4: v4.0.0\n"

	w := syncedMergedWorkspace(t)
	w.replaceInGoMod(t, "go.yaml.in/yaml/v4 v4.0.0", "go.yaml.in/yaml/v4 v4.1.0")
	w.tidyRaises("go.yaml.in/yaml/v4", "v4.0.0", "v4.1.0")

	first, err := w.syncReport(t, declaring)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"golden-go/go.mod: the declared version of go.yaml.in/yaml/v4 is stale. " +
			"forge-factory.yaml declares v4.0.0 and the lock commands raised it to v4.1.0. " +
			"Set go.yaml.in/yaml/v4 to v4.1.0 in forge-factory.yaml.",
	}, notesNaming(first, "go.yaml.in/yaml/v4"))

	second, err := w.syncReport(t, declaring)
	require.NoError(t, err)
	assert.Empty(t, notesNaming(second, "go.yaml.in/yaml/v4"))
}

func TestASyncAfterASyncWhoseTidyRaisedADeclaredVersionStaysGreen(t *testing.T) {
	t.Parallel()

	w := newMergedWorkspace(t)
	w.tidyRaises("sigs.k8s.io/yaml", "v1.6.0", "v1.7.0")

	require.NoError(t, w.sync(t))
	assert.Equal(t, map[string]string{"sigs.k8s.io/yaml": "v1.7.0"}, w.recorded(t, "golden-go/go.mod").Versions,
		"the settle step records the declared version its tidy left on disk")

	require.NoError(t, w.sync(t))
}
