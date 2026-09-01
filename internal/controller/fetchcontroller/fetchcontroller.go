// Package fetchcontroller brings one artifact's bytes to a local path. It
// is the FETCHED arrow of the runtime lifecycle, and the one place source
// customization lives: its rewrite rules retarget url prefixes at a mirror
// or an internal artifact store. The sha256 check is not optional - however
// the bytes arrive, they must hash to the description's pin, so a rewrite
// target needs no trust.
package fetchcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/httpadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
)

// Rewrite retargets one url prefix. The first matching rule wins.
type Rewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Controller struct {
	http httpadapter.Client
	fs   fsadapter.FS
}

func New(http httpadapter.Client, fs fsadapter.FS) *Controller {
	return &Controller{http: http, fs: fs}
}

// Fetch downloads one artifact to dest and answers the hash it verified.
// A mismatch refuses loud and writes nothing: bytes that do not hash to the
// pin are not the artifact, whatever server sent them.
func (c *Controller) Fetch(
	ctx context.Context,
	artifact runtimetypes.Artifact,
	dest string,
	rewrites []Rewrite,
) (string, error) {
	url := artifact.URL

	for _, r := range rewrites {
		if r.From != "" && strings.HasPrefix(url, r.From) {
			url = r.To + strings.TrimPrefix(url, r.From)

			break
		}
	}

	body, err := c.http.Get(ctx, url)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(body)

	got := hex.EncodeToString(sum[:])
	if got != artifact.SHA256 {
		return "", fmt.Errorf(
			"fetched %s: sha256 mismatch: the description pins %s and the bytes hash to %s; "+
				"these are not the pinned bytes, refusing to write them", url, artifact.SHA256, got)
	}

	if err := c.fs.MkdirAll(filepath.Dir(dest)); err != nil {
		return "", fmt.Errorf("preparing %s: %w", dest, err)
	}

	if err := c.fs.WriteFile(dest, body); err != nil {
		return "", fmt.Errorf("writing %s: %w", dest, err)
	}

	return got, nil
}

// ParseRewrites reads the engine's spec block. Unknown keys are refused:
// a typo in a mirror rule is a fetch that silently goes upstream, which is
// exactly what a restricted environment declared the rule to prevent.
func ParseRewrites(spec map[string]any) ([]Rewrite, error) {
	if len(spec) == 0 {
		return nil, nil
	}

	for key := range spec {
		if key != "rewrite" {
			return nil, fmt.Errorf("fetch spec: unknown key %q; the spec holds rewrite and nothing else", key)
		}
	}

	raw, err := json.Marshal(spec["rewrite"])
	if err != nil {
		return nil, fmt.Errorf("reading the rewrite rules: %w", err)
	}

	var rules []Rewrite
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("reading the rewrite rules: %w", err)
	}

	for i, r := range rules {
		if r.From == "" || r.To == "" {
			return nil, fmt.Errorf("rewrite[%d]: both from and to are required", i)
		}
	}

	return rules, nil
}
