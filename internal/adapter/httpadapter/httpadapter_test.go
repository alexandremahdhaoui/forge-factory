package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/httpadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAnswersTheWholeBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("bytes"))
	}))
	t.Cleanup(server.Close)

	body, err := httpadapter.New().Get(context.Background(), server.URL)
	require.NoError(t, err)
	assert.Equal(t, []byte("bytes"), body)
}

func TestANon200NamesTheURLAndStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	_, err := httpadapter.New().Get(context.Background(), server.URL+"/asset")
	require.ErrorContains(t, err, "status 403")
	require.ErrorContains(t, err, server.URL+"/asset")
}

func TestAnUnreachableServerIsAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	_, err := httpadapter.New().Get(context.Background(), server.URL)
	require.ErrorContains(t, err, "fetching")
}

func TestAMalformedURLIsAnError(t *testing.T) {
	t.Parallel()

	_, err := httpadapter.New().Get(context.Background(), "http://\x7f bad url")
	require.ErrorContains(t, err, "building the request")
}
