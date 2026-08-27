//go:build e2e

package e2e_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forge-factory-e2e")
	if err != nil {
		panic(err)
	}

	binDir = dir

	cmd := exec.Command("go", "build", "-o", dir, "./cmd/...")
	cmd.Dir = repoRoot()
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		panic(err)
	}

	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		panic(err)
	}

	code := m.Run()

	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	return filepath.Dir(filepath.Dir(wd))
}

const factory = `version: "1"
name: sample

repos:
  - name: sample-go
    url: git@github.com:x/sample-go.git
    languages: [go]
  - name: sample-rust
    url: git@github.com:x/sample-rust.git
    languages: [rust]
  - name: sample-web
    url: git@github.com:x/sample-web.git
    languages: [typescript]

dependencies:
  go:
    sigs.k8s.io/yaml: v1.6.0
  rust:
    serde: "1"
  typescript:
    neverthrow: ^8

engines:
  - alias: go
    engine: forge://factory-lang-go
  - alias: rust
    engine: forge://factory-lang-rust
  - alias: typescript
    engine: forge://factory-lang-typescript
`

// workspace lays out a factory with three members, each carrying the identity
// its language needs in its own forge.yaml.
func workspace(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	write(t, filepath.Join(root, "forge-factory.yaml"), factory)

	for name, forge := range map[string]string{
		"sample-go":   "name: sample-go\nfactory:\n  module: example.com/sample\n  goVersion: \"1.26\"\n",
		"sample-rust": "name: sample-rust\n",
		"sample-web":  "name: sample-web\nfactory:\n  bin: dist/cmd/web.js\n",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o755))
		write(t, filepath.Join(root, name, "forge.yaml"), forge)
	}

	// A tidy drops a require nothing imports, so the member has to use the
	// dependency the factory names or the bump has nothing to prove.
	write(t, filepath.Join(root, "sample-go", "main.go"), goSource)

	return root
}

const goSource = `package main

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

func main() {
	out, err := yaml.Marshal(map[string]string{"a": "b"})
	if err != nil {
		panic(err)
	}

	fmt.Print(string(out))
}
`

func write(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func read(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(raw)
}

func run(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()

	cmd := exec.Command(filepath.Join(binDir, "forge-factory"), args...)
	cmd.Dir = root

	out, err := cmd.CombinedOutput()

	return string(out), err
}

func mustRun(t *testing.T, root string, args ...string) string {
	t.Helper()

	out, err := run(t, root, args...)
	require.NoError(t, err, out)

	return out
}

func TestSyncMaterialisesEveryLanguage(t *testing.T) {
	root := workspace(t)

	out := mustRun(t, root, "sync")
	assert.Contains(t, out, "wrote go.work")
	assert.Contains(t, out, "wrote Cargo.toml")
	assert.Contains(t, out, "wrote pnpm-workspace.yaml")

	assert.Contains(t, read(t, filepath.Join(root, "sample-go", "go.mod")), "module example.com/sample")
	assert.Contains(t, read(t, filepath.Join(root, "go.work")), "./sample-go")
	assert.Contains(t, read(t, filepath.Join(root, "go.work")), "go 1.26")
	assert.Contains(t, read(t, filepath.Join(root, "Cargo.toml")), `"sample-rust"`)
	assert.Contains(t, read(t, filepath.Join(root, "Cargo.toml")), `serde = "1"`,
		"a bare 1 is not a cargo version")
	assert.Contains(t, read(t, filepath.Join(root, "sample-web", "package.json")), "neverthrow")
	assert.Contains(t, read(t, filepath.Join(root, "pnpm-workspace.yaml")), `- "sample-web"`)
}

func TestEverythingGeneratedIsGitignored(t *testing.T) {
	root := workspace(t)

	mustRun(t, root, "sync")

	assert.Contains(t, read(t, filepath.Join(root, "sample-go", ".gitignore")), "/go.mod")
	assert.Contains(t, read(t, filepath.Join(root, "sample-web", ".gitignore")), "/package.json")

	_, err := os.Stat(filepath.Join(root, "sample-rust", ".gitignore"))
	assert.True(t, os.IsNotExist(err), "cargo needs nothing inside a member, so nothing is ignored there")
}

func TestSyncIsIdempotent(t *testing.T) {
	root := workspace(t)

	mustRun(t, root, "sync")
	before := read(t, filepath.Join(root, "sample-go", ".gitignore"))

	mustRun(t, root, "sync")
	assert.Equal(t, before, read(t, filepath.Join(root, "sample-go", ".gitignore")),
		"a second sync must not append the same line twice")
}

func TestOneBumpMovesEveryGoMemberWithNoCommitInAny(t *testing.T) {
	root := workspace(t)

	mustRun(t, root, "sync")
	require.Contains(t, read(t, filepath.Join(root, "sample-go", "go.mod")), "sigs.k8s.io/yaml v1.6.0")

	// Both versions exist. A version nobody can resolve is its own test below,
	// because the tidy fails and the sync must say so.
	out := mustRun(t, root, "bump", "go:sigs.k8s.io/yaml", "v1.5.0")
	assert.Contains(t, out, "now sigs.k8s.io/yaml: v1.5.0")

	assert.Contains(t, read(t, filepath.Join(root, "sample-go", "go.mod")), "sigs.k8s.io/yaml v1.5.0")
	assert.Contains(t, read(t, filepath.Join(root, "forge-factory.yaml")), "sigs.k8s.io/yaml: v1.5.0")
}

// A sync that writes a version nothing can resolve leaves every member
// unbuildable. It used to report that and exit zero, which is a lie.
func TestABumpToAVersionNobodyCanResolveFails(t *testing.T) {
	root := workspace(t)

	mustRun(t, root, "sync")

	out, err := run(t, root, "bump", "go:sigs.k8s.io/yaml", "v1.99.0")
	require.Error(t, err, out)
	assert.Contains(t, out, "which a build will need")

	// The proxy's exact words differ by environment: a direct fetch says
	// "unknown revision", a filtered one says "Forbidden" on the fallback.
	// What matters is that the reason reached the report.
	if !strings.Contains(out, "unknown revision") && !strings.Contains(out, "unrecognized import path") {
		t.Fatalf("the reason the build cannot be settled is not on the report:\n%s", out)
	}
}

func TestOfflineAcceptsASyncThatCouldNotSettle(t *testing.T) {
	root := workspace(t)

	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "--offline", "go:sigs.k8s.io/yaml", "v1.99.0")

	assert.Contains(t, read(t, filepath.Join(root, "forge-factory.yaml")), "v1.99.0")
}

func TestAddPutsANewMemberInEveryMembershipList(t *testing.T) {
	root := workspace(t)

	mustRun(t, root, "sync")

	for _, list := range []string{"go.work", "Cargo.toml", "pnpm-workspace.yaml"} {
		require.NotContains(t, read(t, filepath.Join(root, list)), "sample-two", list)
	}

	require.NoError(t, os.MkdirAll(filepath.Join(root, "sample-two"), 0o755))
	write(t, filepath.Join(root, "sample-two", "forge.yaml"),
		"name: sample-two\nfactory:\n  module: example.com/two\n")

	mustRun(t, root, "add", "sample-two",
		"git@github.com:x/sample-two.git", "go", "rust", "typescript")

	// One add, and all three membership lists move together. That is the whole
	// point: membership used to be declared five times and no two agreed.
	assert.Contains(t, read(t, filepath.Join(root, "go.work")), "./sample-two")
	assert.Contains(t, read(t, filepath.Join(root, "Cargo.toml")), `"sample-two"`)
	assert.Contains(t, read(t, filepath.Join(root, "pnpm-workspace.yaml")), `- "sample-two"`)
	assert.Contains(t, read(t, filepath.Join(root, "sample-two", "go.mod")), "module example.com/two")
}

func TestADeletedManifestComesBack(t *testing.T) {
	root := workspace(t)

	mustRun(t, root, "sync")
	require.NoError(t, os.Remove(filepath.Join(root, "sample-go", "go.mod")))

	mustRun(t, root, "sync")
	assert.Contains(t, read(t, filepath.Join(root, "sample-go", "go.mod")), "module example.com/sample")
}

func TestValidateDescribesTheFactoryWithoutTouchingDisk(t *testing.T) {
	root := workspace(t)

	out := mustRun(t, root, "validate")
	assert.Contains(t, out, "sample: 3 repos, 3 engines, 3 languages")

	_, err := os.Stat(filepath.Join(root, "go.work"))
	assert.True(t, os.IsNotExist(err))
}

func TestStatusReportsMembersThatAreNotCheckedOut(t *testing.T) {
	root := workspace(t)

	out, err := run(t, root, "status")
	require.Error(t, err, "status fails when the workspace disagrees, so a gate can use it")
	assert.Contains(t, out, "sample-go is a directory and not a git repo")
}

func TestStatusAgreesOnceEveryMemberIsARepo(t *testing.T) {
	root := workspace(t)

	for _, name := range []string{"sample-go", "sample-rust", "sample-web"} {
		gitInit(t, filepath.Join(root, name))
	}

	out := mustRun(t, root, "status")
	assert.Contains(t, out, "sample-go")
	assert.NotContains(t, out, "is missing")
}

func TestSyncRefusesAMemberMissingTheIdentityItsLanguageNeeds(t *testing.T) {
	root := workspace(t)

	write(t, filepath.Join(root, "sample-go", "forge.yaml"), "name: sample-go\n")

	out, err := run(t, root, "sync")
	require.Error(t, err)
	assert.Contains(t, out, "factory.module")
}

func TestSyncRefusesAFactoryThatDoesNotValidate(t *testing.T) {
	root := workspace(t)

	write(t, filepath.Join(root, "forge-factory.yaml"), strings.Replace(factory,
		"  - alias: rust\n    engine: forge://factory-lang-rust\n", "", 1))

	out, err := run(t, root, "sync")
	require.Error(t, err)
	assert.Contains(t, out, `repos declare the language "rust" and no engine has that alias`)
}

func TestForgeFactoryWithNoVerbPrintsItsUsage(t *testing.T) {
	root := workspace(t)

	out, err := run(t, root)
	require.Error(t, err)
	assert.Contains(t, out, "usage: forge-factory")
}

func gitInit(t *testing.T, dir string) {
	t.Helper()

	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		// A machine's global signing config must not reach test repos.
		{"config", "tag.gpgsign", "false"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "--quiet", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
}

// TestAGeneratedGoModBuilds is the point of the whole tool. A member carries no
// go.mod of its own, forge-factory writes one from the shared versions, and the
// package compiles against a dependency the factory alone named.
//
// The tidy runs with GOWORK=off. In workspace mode a tidy writes no per module
// sums, so the member would carry a go.mod naming versions and nothing proving
// them.
func TestAGeneratedGoModBuilds(t *testing.T) {
	root := workspace(t)

	out := mustRun(t, root, "sync")
	require.Contains(t, out, "ran go mod tidy", out)

	mod := read(t, filepath.Join(root, "sample-go", "go.mod"))
	assert.Contains(t, mod, "sigs.k8s.io/yaml v1.6.0")

	_, err := os.Stat(filepath.Join(root, "sample-go", "go.sum"))
	require.NoError(t, err,
		"the tidy runs with GOWORK=off, so the member gets the sums a build needs")

	assert.Contains(t, read(t, filepath.Join(root, "sample-go", ".gitignore")), "/go.sum",
		"a derived file is never committed either")

	build := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "out"), ".")
	build.Dir = filepath.Join(root, "sample-go")

	logs, err := build.CombinedOutput()
	require.NoError(t, err, string(logs))
}

// TestCheckoutPutsAMemberOnItsProvenSHA runs against a real state engine over
// MCP. forge-factory speaks the transport in forge-revision-spec and imports
// forge-ci nowhere, so any conforming engine works here.
func TestCheckoutPutsAMemberOnItsProvenSHA(t *testing.T) {
	engine, err := exec.LookPath("ci-state-git")
	if err != nil {
		t.Skip("no state engine on PATH, so the transport cannot be exercised")
	}

	root := workspace(t)
	repo := filepath.Join(root, "sample-go")
	gitInit(t, repo)

	first := headSHA(t, repo)

	write(t, filepath.Join(repo, "later.go"), "package main\n")
	commitAll(t, repo)

	require.NotEqual(t, first, headSHA(t, repo))

	store := filepath.Join(root, "state")
	writeRevision(t, store, "proven", first)

	write(t, filepath.Join(root, "forge-factory.yaml"), factory+`
state:
  engine: forge://`+filepath.Base(engine)+`
  spec:
    path: `+store+`
`)

	out := mustRun(t, root, "checkout", "proven")
	assert.Contains(t, out, "revision proven")

	assert.Equal(t, first, headSHA(t, repo), "the member sits on the SHA the revision proved")
	assert.Contains(t, out, "wrote go.work", "a checkout syncs to match")
}

func TestCheckoutRefusesToDestroyUncommittedWork(t *testing.T) {
	engine, err := exec.LookPath("ci-state-git")
	if err != nil {
		t.Skip("no state engine on PATH, so the transport cannot be exercised")
	}

	root := workspace(t)
	repo := filepath.Join(root, "sample-go")
	gitInit(t, repo)

	first := headSHA(t, repo)

	write(t, filepath.Join(repo, "later.go"), "package main\n")
	commitAll(t, repo)
	write(t, filepath.Join(repo, "main.go"), goSource+"\n// edited\n")

	store := filepath.Join(root, "state")
	writeRevision(t, store, "proven", first)

	write(t, filepath.Join(root, "forge-factory.yaml"), factory+`
state:
  engine: forge://`+filepath.Base(engine)+`
  spec:
    path: `+store+`
`)

	out, err := run(t, root, "checkout", "proven")
	require.Error(t, err)
	assert.Contains(t, out, "uncommitted changes")
	assert.NotEqual(t, first, headSHA(t, repo), "nothing moved")
}

func TestCheckoutReportsARevisionNobodyMinted(t *testing.T) {
	engine, err := exec.LookPath("ci-state-git")
	if err != nil {
		t.Skip("no state engine on PATH, so the transport cannot be exercised")
	}

	root := workspace(t)

	write(t, filepath.Join(root, "forge-factory.yaml"), factory+`
state:
  engine: forge://`+filepath.Base(engine)+`
  spec:
    path: `+filepath.Join(root, "state")+`
`)

	out, err := run(t, root, "checkout", "never-minted")
	require.Error(t, err)
	assert.Contains(t, out, "no such revision")
}

func writeRevision(t *testing.T, store, id, sha string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(store, "revisions"), 0o755))
	write(t, filepath.Join(store, "revisions", id+".json"), fmt.Sprintf(
		`{"id":%q,"createdAt":"2026-08-19T00:00:00Z","repos":{"sample-go":%q}}`, id, sha))
}

func headSHA(t *testing.T, dir string) string {
	t.Helper()

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	return strings.TrimSpace(string(out))
}

func commitAll(t *testing.T, dir string) {
	t.Helper()

	for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "more"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
}
