//go:build e2e

package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildToyProvider puts the fixture provider engine beside the real
// binaries, once for the whole suite - it resolves by name on PATH exactly
// as factory-fetch and factory-install do.
var buildToyProvider = sync.OnceValue(func() error {
	cmd := exec.Command("go", "build", "-o", binDir, "./test/e2e/testdata/toy-provider")
	cmd.Dir = repoRoot()
	cmd.Stderr = os.Stderr

	return cmd.Run()
})

// toyArchive builds the runtime's tar.gz: one executable that proves it ran
// from the store, plus the env file's variable.
func toyArchive(t *testing.T) []byte {
	t.Helper()

	var tarBuf bytes.Buffer

	w := tar.NewWriter(&tarBuf)

	script := "#!/bin/sh\necho \"toygo 1.0.0 TOY_HOME=$TOY_HOME\"\n"
	require.NoError(t, w.WriteHeader(&tar.Header{
		Name: "toy-1.0.0/bin/toygo", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg,
	}))

	_, err := w.Write([]byte(script))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	var gzBuf bytes.Buffer

	gz := gzip.NewWriter(&gzBuf)
	_, err = gz.Write(tarBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	return gzBuf.Bytes()
}

// runtimeWorkspace stands up a factory declaring one runtime served by the
// fixture provider, with the fetch engine's rewrite pointing the upstream
// url at the given mirror - the exact mirror mechanism a restricted
// environment declares.
func runtimeWorkspace(t *testing.T, mirror string, archive []byte, prereqs string) string {
	t.Helper()

	require.NoError(t, buildToyProvider())

	root := t.TempDir()

	sum := sha256.Sum256(archive)

	description := map[string]any{
		"runtime": "toygo",
		"version": "1.0.0",
		"artifacts": []map[string]any{{
			"url":    "https://runtime.invalid/toy-1.0.0.tar.gz",
			"sha256": hex.EncodeToString(sum[:]),
			"unpack": "tar-gz",
			"strip":  1,
		}},
		"bins": []string{"bin/toygo"},
		"env":  map[string]string{"TOY_HOME": "{prefix}"},
	}

	if prereqs != "" {
		var parsed []map[string]any

		require.NoError(t, json.Unmarshal([]byte(prereqs), &parsed))

		description["prerequisites"] = parsed
	}

	raw, err := json.MarshalIndent(description, "", "  ")
	require.NoError(t, err)

	descPath := filepath.Join(root, "toy-description.json")
	write(t, descPath, string(raw))
	t.Setenv("TOY_DESCRIPTION", descPath)
	t.Setenv("FORGE_STORE_DIR", filepath.Join(root, "store"))

	factory := fmt.Sprintf(`version: "1"
name: runtime-sample

repos:
  - name: sample-spec
    url: git@github.com:x/sample-spec.git

engines:
  - alias: toygo
    engine: forge://toy-provider
  - alias: fetch
    engine: forge://factory-fetch
    spec:
      rewrite:
        - from: https://runtime.invalid/
          to: %s/
  - alias: install
    engine: forge://factory-install

toolchain:
  runtimes:
    toygo: { version: "1.0.0" }
`, mirror)

	write(t, filepath.Join(root, "forge-factory.yaml"), factory)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sample-spec"), 0o755))
	write(t, filepath.Join(root, "sample-spec", "forge.yaml"), "name: sample-spec\n")

	return root
}

// The whole lifecycle against real engines over real MCP: the provider
// describes, the fetch engine pulls hash-verified bytes through the mirror
// rewrite, the install engine lays them out contained, and the store's
// executable answers from the workspace's .forge/bin with its env set.
func TestSyncProvisionsADeclaredRuntime(t *testing.T) {
	archive := toyArchive(t)

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(mirror.Close)

	root := runtimeWorkspace(t, mirror.URL, archive, "")

	out := mustRun(t, root, "sync")
	assert.Contains(t, out, "installed toygo@1.0.0")

	// The exposed executable runs from the store, and the managed .envrc
	// block sources the composed environment.
	bin := filepath.Join(root, ".forge", "bin", "toygo")
	envrc := read(t, filepath.Join(root, "sample-spec", ".envrc"))
	require.Contains(t, envrc, ".forge/env")

	cmd := exec.Command("sh", "-c", ". "+filepath.Join(root, ".forge", "env")+" && "+bin)
	probe, err := cmd.CombinedOutput()
	require.NoError(t, err, string(probe))
	assert.Contains(t, string(probe), "toygo 1.0.0")
	assert.Contains(t, string(probe), filepath.Join("store", "runtimes", "toygo@1.0.0"),
		"TOY_HOME points into the content-addressed store")

	// A second sync converges: the store is warm, nothing is fetched, and
	// the report says reused.
	again := mustRun(t, root, "sync")
	assert.Contains(t, again, "reused toygo@1.0.0")
	assert.NotContains(t, again, "installed toygo@1.0.0")
}

// Bytes that do not hash to the description's pin are refused loud and the
// store stays empty - the mirror needs no trust because the hash decides.
func TestATamperedRuntimeArchiveIsRefused(t *testing.T) {
	archive := toyArchive(t)

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered bytes, same url"))
	}))
	t.Cleanup(mirror.Close)

	root := runtimeWorkspace(t, mirror.URL, archive, "")

	out, err := run(t, root, "sync")
	require.Error(t, err, out)
	assert.Contains(t, out, "refusing")

	// Only the advisory lock file may exist; no runtime directory landed.
	entries, _ := os.ReadDir(filepath.Join(root, "store", "runtimes"))
	for _, e := range entries {
		assert.False(t, e.IsDir(), "nothing may land from bytes that failed the pin: %s", e.Name())
		assert.True(t, strings.HasSuffix(e.Name(), ".lock"), e.Name())
	}
}

// A prerequisite nothing satisfies fails before anything downloads, naming
// the capability, the reason and the avenues.
func TestAMissingRuntimePrerequisiteFailsTheSyncLoud(t *testing.T) {
	archive := toyArchive(t)

	var served int

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++

		_, _ = w.Write(archive)
	}))
	t.Cleanup(mirror.Close)

	root := runtimeWorkspace(t, mirror.URL, archive,
		`[{"name":"quantum-linker","reason":"the fixture demands it","verify":"definitely-not-installed-xyz"}]`)

	out, err := run(t, root, "sync")
	require.Error(t, err, out)
	assert.Contains(t, out, "quantum-linker")
	assert.Contains(t, out, "the fixture demands it")
	assert.Contains(t, out, "declare a runtime whose provides lists")
	assert.Zero(t, served, "a doomed provision downloads nothing")
}

// The e2e run must never see this test leak PATH state between cases; the
// provisioner prepends the workspace bin to the test process's own exec'd
// child only, so the suite's PATH is untouched by design - pinned here.
func TestProvisioningDoesNotMutateTheSuitePath(t *testing.T) {
	before := os.Getenv("PATH")

	archive := toyArchive(t)

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(mirror.Close)

	root := runtimeWorkspace(t, mirror.URL, archive, "")
	mustRun(t, root, "sync")

	assert.True(t, strings.HasPrefix(os.Getenv("PATH"), strings.Split(before, string(os.PathListSeparator))[0]))
}
