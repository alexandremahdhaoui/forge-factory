package repoadapter

import (
	"fmt"
	"path/filepath"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"sigs.k8s.io/yaml"
)

// Reader answers what a repo declares about itself.
type Reader interface {
	Identity(repoPath string) (map[string]string, error)
}

// ForgeYAML reads the factory block of a repo's forge.yaml. forge ignores keys
// it does not know, so the block costs that repo nothing.
type ForgeYAML struct {
	fs fsadapter.FS
}

var _ Reader = ForgeYAML{}

func New(fs fsadapter.FS) ForgeYAML {
	return ForgeYAML{fs: fs}
}

func (r ForgeYAML) Identity(repoPath string) (map[string]string, error) {
	path := filepath.Join(repoPath, "forge.yaml")

	exists, err := r.fs.Exists(path)
	if err != nil {
		return nil, err
	}

	if !exists {
		return map[string]string{}, nil
	}

	raw, err := r.fs.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc struct {
		Factory map[string]string `json:"factory"`
	}

	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reading the factory block of %s: %w", path, err)
	}

	if doc.Factory == nil {
		return map[string]string{}, nil
	}

	return doc.Factory, nil
}
