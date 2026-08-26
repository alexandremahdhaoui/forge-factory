// Package distadapter fetches distribution files - the index and the
// binaries it names - from wherever they live: a local directory (an
// airgapped mirror, a pipeline's staging dir) or an http(s) base URL (a
// release's download prefix). The caller verifies digests; this adapter
// only moves bytes.
package distadapter

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Source fetches one file by its name relative to the distribution base.
type Source struct {
	base   string
	client *http.Client
}

// New builds a source over a directory path or an http(s) base URL.
func New(base string) Source {
	return Source{base: base, client: &http.Client{Timeout: 5 * time.Minute}}
}

// Fetch answers the named file's bytes.
func (s Source) Fetch(name string) ([]byte, error) {
	if strings.HasPrefix(s.base, "http://") || strings.HasPrefix(s.base, "https://") {
		return s.fetchHTTP(strings.TrimSuffix(s.base, "/") + "/" + name)
	}

	data, err := os.ReadFile(filepath.Join(s.base, name))
	if err != nil {
		return nil, fmt.Errorf("reading %s from %s: %w", name, s.base, err)
	}

	return data, nil
}

func (s Source) fetchHTTP(url string) ([]byte, error) {
	resp, err := s.client.Get(url) //nolint:noctx // the client carries the timeout
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}

	return data, nil
}
