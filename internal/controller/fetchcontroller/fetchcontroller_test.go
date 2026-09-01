package fetchcontroller_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/httpadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/fetchcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sum(data []byte) string {
	s := sha256.Sum256(data)

	return hex.EncodeToString(s[:])
}

func TestFetchVerifiesAndWrites(t *testing.T) {
	t.Parallel()

	body := []byte("the pinned bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	dest := filepath.Join(t.TempDir(), "sub", "artifact")
	c := fetchcontroller.New(httpadapter.New(), fsadapter.New())

	got, err := c.Fetch(context.Background(),
		runtimetypes.Artifact{URL: server.URL + "/a", SHA256: sum(body)}, dest, nil)
	require.NoError(t, err)
	assert.Equal(t, sum(body), got)

	raw, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, body, raw)
}

// The whole trust model: bytes that do not hash to the pin are refused and
// nothing lands, whatever server sent them.
func TestAMismatchRefusesAndWritesNothing(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	t.Cleanup(server.Close)

	dest := filepath.Join(t.TempDir(), "artifact")
	c := fetchcontroller.New(httpadapter.New(), fsadapter.New())

	_, err := c.Fetch(context.Background(),
		runtimetypes.Artifact{URL: server.URL + "/a", SHA256: sum([]byte("the pinned bytes"))}, dest, nil)
	require.ErrorContains(t, err, "refusing to write them")

	_, statErr := os.Stat(dest)
	assert.True(t, os.IsNotExist(statErr))
}

// A rewrite retargets the prefix - the mirror and airgap door - and the
// hash still decides, so the mirror needs no trust.
func TestARewriteRetargetsTheURL(t *testing.T) {
	t.Parallel()

	body := []byte("mirrored bytes")

	var asked string

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		_, _ = w.Write(body)
	}))
	t.Cleanup(mirror.Close)

	c := fetchcontroller.New(httpadapter.New(), fsadapter.New())

	_, err := c.Fetch(context.Background(),
		runtimetypes.Artifact{URL: "https://upstream.example.com/dist/a.tar.gz", SHA256: sum(body)},
		filepath.Join(t.TempDir(), "a.tar.gz"),
		[]fetchcontroller.Rewrite{{From: "https://upstream.example.com/", To: mirror.URL + "/"}})
	require.NoError(t, err)
	assert.Equal(t, "/dist/a.tar.gz", asked, "the path survives; only the prefix is retargeted")
}

// A mirror answering 404 with an HTML body must never reach the hash check
// as content.
func TestANon200IsAnErrorNotContent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html>not found</html>", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	c := fetchcontroller.New(httpadapter.New(), fsadapter.New())

	_, err := c.Fetch(context.Background(),
		runtimetypes.Artifact{URL: server.URL + "/gone", SHA256: sum([]byte("x"))},
		filepath.Join(t.TempDir(), "gone"), nil)
	require.ErrorContains(t, err, "status 404")
}

func TestParseRewrites(t *testing.T) {
	t.Parallel()

	rules, err := fetchcontroller.ParseRewrites(map[string]any{
		"rewrite": []any{map[string]any{"from": "https://a/", "to": "https://b/"}},
	})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, fetchcontroller.Rewrite{From: "https://a/", To: "https://b/"}, rules[0])

	empty, err := fetchcontroller.ParseRewrites(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// A typo'd key is a fetch that silently goes upstream - the exact thing a
// restricted environment declared the rule to prevent - so it refuses.
func TestParseRewritesRefusesUnknownKeysAndHalfRules(t *testing.T) {
	t.Parallel()

	_, err := fetchcontroller.ParseRewrites(map[string]any{"rewrites": []any{}})
	require.ErrorContains(t, err, `unknown key "rewrites"`)

	_, err = fetchcontroller.ParseRewrites(map[string]any{
		"rewrite": []any{map[string]any{"from": "https://a/"}},
	})
	require.ErrorContains(t, err, "both from and to are required")

	_, err = fetchcontroller.ParseRewrites(map[string]any{"rewrite": "not-a-list"})
	require.ErrorContains(t, err, "reading the rewrite rules")
}

func TestAnUnreachableSourceIsAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	c := fetchcontroller.New(httpadapter.New(), fsadapter.New())

	_, err := c.Fetch(context.Background(),
		runtimetypes.Artifact{URL: server.URL + "/a", SHA256: sum([]byte("x"))},
		filepath.Join(t.TempDir(), "a"), nil)
	require.Error(t, err)
}

// The write half fails loud too - a full disk must not read as a fetch.
func TestAFailingWriteIsAnError(t *testing.T) {
	t.Parallel()

	body := []byte("bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	c := fetchcontroller.New(httpadapter.New(), fsadapter.New())

	// The dest's parent is a FILE, so neither the mkdir nor the write can land.
	parent := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))

	_, err := c.Fetch(context.Background(),
		runtimetypes.Artifact{URL: server.URL + "/a", SHA256: sum(body)},
		filepath.Join(parent, "artifact"), nil)
	require.ErrorContains(t, err, "preparing")
}
