package distadapter_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/distadapter"
)

func TestADirectorySourceReadsFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"revision":"x"}`), 0o600))

	data, err := distadapter.New(dir).Fetch("index.json")
	require.NoError(t, err)
	assert.Equal(t, `{"revision":"x"}`, string(data))

	_, err = distadapter.New(dir).Fetch("missing")
	require.ErrorContains(t, err, "reading missing")
}

func TestAnHTTPSourceFetches(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dist/index.json" {
			_, _ = w.Write([]byte(`{"revision":"y"}`))

			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()

	data, err := distadapter.New(server.URL + "/dist/").Fetch("index.json")
	require.NoError(t, err)
	assert.Equal(t, `{"revision":"y"}`, string(data))

	_, err = distadapter.New(server.URL + "/dist").Fetch("missing")
	require.ErrorContains(t, err, "404")
}

func TestAnUnreachableHTTPSourceFailsLoud(t *testing.T) {
	t.Parallel()

	_, err := distadapter.New("http://127.0.0.1:1/nowhere").Fetch("index.json")
	require.ErrorContains(t, err, "fetching")
}
