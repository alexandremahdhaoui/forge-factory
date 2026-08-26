package toolingcontroller_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/toolingcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/disttypes"
)

// mapSource is a distribution held in memory: index.json plus assets.
type mapSource map[string][]byte

func (m mapSource) Fetch(name string) ([]byte, error) {
	data, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("no %s in the source", name)
	}

	return data, nil
}

// distribution builds a source carrying one revision with the given tools,
// each asset digested honestly.
func distribution(t *testing.T, revision string, tools map[string]string) mapSource {
	t.Helper()

	source := mapSource{}
	index := disttypes.Index{Revision: revision, Tools: []disttypes.Tool{}}

	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}

	// Deterministic order keeps reports comparable.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	for _, name := range names {
		content := []byte(tools[name])
		sum := sha256.Sum256(content)
		asset := name + "_linux_amd64"
		source[asset] = content

		index.Tools = append(index.Tools, disttypes.Tool{
			Name: name,
			Platforms: map[string]disttypes.Platform{
				"linux/amd64": {
					Digest: "sha256:" + hex.EncodeToString(sum[:]),
					Size:   int64(len(content)),
					Asset:  asset,
				},
			},
		})
	}

	raw, err := json.Marshal(index)
	require.NoError(t, err)
	source["index.json"] = raw

	return source
}

func apply(t *testing.T, req toolingcontroller.Request) (toolingcontroller.Report, error) {
	t.Helper()

	return toolingcontroller.New(fsadapter.New()).Apply(req)
}

func TestApplyPopulatesTheStoreAndLinksTheWorkspace(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	root := t.TempDir()
	source := distribution(t, "abc123def456", map[string]string{
		"forge":         "#!/bin/sh\necho forge\n",
		"forge-factory": "#!/bin/sh\necho forge-factory\n",
	})

	report, err := apply(t, toolingcontroller.Request{
		Root: root, StoreDir: store, Source: source, SourceName: "test", Platform: "linux/amd64",
	})
	require.NoError(t, err)

	assert.Equal(t, "abc123def456", report.Revision)
	assert.Equal(t, []string{"forge", "forge-factory"}, report.Installed)
	assert.Empty(t, report.Reused)

	// The blob is immutable and executable.
	view := filepath.Join(store, "rev", "abc123def456", "linux-amd64", "bin", "forge")
	content, err := os.ReadFile(view)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho forge\n", string(content))

	info, err := os.Stat(view)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "a distributed binary must execute")

	// The workspace bin resolves through the view, and the pin is recorded.
	wsBinary, err := os.ReadFile(filepath.Join(root, ".forge", "bin", "forge"))
	require.NoError(t, err)
	assert.Equal(t, string(content), string(wsBinary))

	pin, err := os.ReadFile(filepath.Join(root, ".forge", "tooling.json"))
	require.NoError(t, err)
	assert.Contains(t, string(pin), "abc123def456")

	// The revision view records the index that produced it.
	_, err = os.Stat(filepath.Join(store, "rev", "abc123def456", "index.json"))
	require.NoError(t, err)
}

// A second apply reuses every blob: the store is content addressed and a
// warm CI cache downloads nothing.
func TestApplyIsIdempotentAndReusesBlobs(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	root := t.TempDir()
	source := distribution(t, "abc123def456", map[string]string{"forge": "binary-one"})

	req := toolingcontroller.Request{
		Root: root, StoreDir: store, Source: source, SourceName: "test", Platform: "linux/amd64",
	}

	_, err := apply(t, req)
	require.NoError(t, err)

	report, err := apply(t, req)
	require.NoError(t, err)
	assert.Empty(t, report.Installed)
	assert.Equal(t, []string{"forge"}, report.Reused)
}

// Two revisions coexist: nothing is overwritten, both views stay valid.
func TestTwoRevisionsCoexistInOneStore(t *testing.T) {
	t.Parallel()

	store := t.TempDir()

	_, err := apply(t, toolingcontroller.Request{
		StoreDir: store, Platform: "linux/amd64",
		Source: distribution(t, "aaaaaaaaaaaa", map[string]string{"forge": "old"}),
	})
	require.NoError(t, err)

	_, err = apply(t, toolingcontroller.Request{
		StoreDir: store, Platform: "linux/amd64",
		Source: distribution(t, "bbbbbbbbbbbb", map[string]string{"forge": "new"}),
	})
	require.NoError(t, err)

	oldBin, err := os.ReadFile(filepath.Join(store, "rev", "aaaaaaaaaaaa", "linux-amd64", "bin", "forge"))
	require.NoError(t, err)
	assert.Equal(t, "old", string(oldBin))

	newBin, err := os.ReadFile(filepath.Join(store, "rev", "bbbbbbbbbbbb", "linux-amd64", "bin", "forge"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(newBin))
}

// A tampered asset fails loud with nothing landed: the digest is the trust
// anchor and a binary the pipeline did not prove never reaches the store.
func TestATamperedAssetFailsLoudAndLandsNothing(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	source := distribution(t, "abc123def456", map[string]string{"forge": "the-proven-binary"})
	source["forge_linux_amd64"] = []byte("something else entirely")

	_, err := apply(t, toolingcontroller.Request{
		StoreDir: store, Source: source, Platform: "linux/amd64",
	})
	require.ErrorContains(t, err, "refusing a binary the pipeline did not prove")

	entries, _ := os.ReadDir(filepath.Join(store, "blobs", "sha256"))
	assert.Empty(t, entries, "a refused blob must not land")
}

func TestAWrongSizeFailsLoud(t *testing.T) {
	t.Parallel()

	source := distribution(t, "abc123def456", map[string]string{"forge": "content"})

	var index disttypes.Index
	require.NoError(t, json.Unmarshal(source["index.json"], &index))

	entry := index.Tools[0].Platforms["linux/amd64"]
	entry.Size = 999
	index.Tools[0].Platforms["linux/amd64"] = entry

	raw, err := json.Marshal(index)
	require.NoError(t, err)
	source["index.json"] = raw

	_, err = apply(t, toolingcontroller.Request{
		StoreDir: t.TempDir(), Source: source, Platform: "linux/amd64",
	})
	require.ErrorContains(t, err, "bytes")
}

// A distribution that skips this machine's platform is broken for it, and
// says so instead of provisioning half a toolchain.
func TestAMissingPlatformFailsLoud(t *testing.T) {
	t.Parallel()

	source := distribution(t, "abc123def456", map[string]string{"forge": "content"})

	_, err := apply(t, toolingcontroller.Request{
		StoreDir: t.TempDir(), Source: source, Platform: "plan9/mips",
	})
	require.ErrorContains(t, err, "no binary for plan9/mips")
}

func TestBrokenIndexesFailLoud(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("x"))
	goodDigest := "sha256:" + hex.EncodeToString(sum[:])

	cases := map[string]struct {
		source mapSource
		want   string
	}{
		"no source":     {source: nil, want: "no distribution source"},
		"missing index": {source: mapSource{}, want: "fetching the distribution index"},
		"bad json":      {source: mapSource{"index.json": []byte("{")}, want: "parsing the distribution index"},
		"no revision": {
			source: mapSource{"index.json": []byte(`{"tools":[{"name":"x"}]}`)},
			want:   "names no revision",
		},
		"dirty revision": {
			source: mapSource{"index.json": []byte(`{"revision":"abc-dirty","tools":[{"name":"x"}]}`)},
			want:   "dirty revision",
		},
		"no tools": {
			source: mapSource{"index.json": []byte(`{"revision":"abc123def456","tools":[]}`)},
			want:   "names no tools",
		},
		"bad digest": {
			source: mapSource{"index.json": []byte(`{"revision":"abc123def456","tools":[{"name":"x","platforms":{"linux/amd64":{"digest":"md5:nope","asset":"x"}}}]}`)},
			want:   "sha256:<64 hex>",
		},
		"no asset": {
			source: mapSource{"index.json": []byte(fmt.Sprintf(
				`{"revision":"abc123def456","tools":[{"name":"x","platforms":{"linux/amd64":{"digest":"%s"}}}]}`,
				goodDigest))},
			want: "names no asset",
		},
		"asset unfetchable": {
			source: mapSource{"index.json": []byte(fmt.Sprintf(
				`{"revision":"abc123def456","tools":[{"name":"x","platforms":{"linux/amd64":{"digest":"%s","asset":"gone"}}}]}`,
				goodDigest))},
			want: "fetching gone",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var source toolingcontroller.Fetcher
			if tc.source != nil {
				source = tc.source
			}

			_, err := apply(t, toolingcontroller.Request{
				StoreDir: t.TempDir(), Source: source, Platform: "linux/amd64",
			})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// FORGE_STORE_DIR names the store when the request does not.
func TestTheStoreDirComesFromTheEnvironment(t *testing.T) {
	store := t.TempDir()
	t.Setenv("FORGE_STORE_DIR", store)

	source := distribution(t, "abc123def456", map[string]string{"forge": "content"})

	_, err := apply(t, toolingcontroller.Request{Source: source, Platform: "linux/amd64"})
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(store, "rev", "abc123def456", "index.json"))
	require.NoError(t, err)
}
