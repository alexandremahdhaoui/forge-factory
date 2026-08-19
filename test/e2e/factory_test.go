//go:build e2e

package e2e_test

import (
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
    engine: go://factory-lang-go
  - alias: rust
    engine: go://factory-lang-rust
  - alias: typescript
    engine: go://factory-lang-typescript
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

	return root
}

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
	assert.Contains(t, read(t, filepath.Join(root, "Cargo.toml")), "serde = 1")
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

	out := mustRun(t, root, "bump", "go:sigs.k8s.io/yaml", "v1.7.0")
	assert.Contains(t, out, "now sigs.k8s.io/yaml: v1.7.0")

	assert.Contains(t, read(t, filepath.Join(root, "sample-go", "go.mod")), "sigs.k8s.io/yaml v1.7.0")
	assert.Contains(t, read(t, filepath.Join(root, "forge-factory.yaml")), "sigs.k8s.io/yaml: v1.7.0")
}

func TestAddPutsANewMemberInEveryMembershipList(t *testing.T) {
	root := workspace(t)

	mustRun(t, root, "sync")
	require.NotContains(t, read(t, filepath.Join(root, "go.work")), "sample-two")

	require.NoError(t, os.MkdirAll(filepath.Join(root, "sample-two"), 0o755))
	write(t, filepath.Join(root, "sample-two", "forge.yaml"),
		"name: sample-two\nfactory:\n  module: example.com/two\n")

	mustRun(t, root, "add", "sample-two", "git@github.com:x/sample-two.git", "go")

	assert.Contains(t, read(t, filepath.Join(root, "go.work")), "./sample-two")
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
		"  - alias: rust\n    engine: go://factory-lang-rust\n", "", 1))

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
		{"add", "-A"},
		{"commit", "--quiet", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
}
