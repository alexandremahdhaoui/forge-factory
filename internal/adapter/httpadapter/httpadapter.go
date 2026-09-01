// Package httpadapter is the one place forge-factory's engines reach the
// network for artifact bytes. It fetches and answers bytes; every decision
// about urls, hashes and destinations stays in the controllers.
package httpadapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Client is what a controller needs from the network, declared here because
// this package implements it.
type Client interface {
	Get(ctx context.Context, url string) ([]byte, error)
}

// HTTP is the real client.
type HTTP struct {
	client *http.Client
}

var _ Client = (*HTTP)(nil)

// New builds the client. No client-side timeout: a runtime archive is tens
// of megabytes on an arbitrary link, and the caller's context is the
// cancellation.
func New() *HTTP {
	return &HTTP{client: &http.Client{}}
}

// Get answers the body of one 200 response, whole. Anything else is an
// error naming the url and the status, because a mirror answering 404 with
// an HTML body must never reach a hash check as "content".
func (h *HTTP) Get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building the request for %s: %w", url, err)
	}

	res, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: status %d", url, res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}

	return body, nil
}
