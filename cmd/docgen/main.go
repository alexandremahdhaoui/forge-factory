package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/specadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/docscontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/docstypes"
	"sigs.k8s.io/yaml"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fs := fsadapter.New()

	var concepts docstypes.Concepts

	if err := read(fs, "docs/concepts.yaml", &concepts); err != nil {
		return err
	}

	var decisions docstypes.Decisions

	if err := read(fs, "docs/decisions.yaml", &decisions); err != nil {
		return err
	}

	conceptsFile, err := docscontroller.RenderConcepts(concepts)
	if err != nil {
		return err
	}

	decisionsFile, err := docscontroller.RenderDecisions(decisions)
	if err != nil {
		return err
	}

	files := []docstypes.File{conceptsFile, decisionsFile}

	engineFiles, err := engineDocs(fs)
	if err != nil {
		return err
	}

	files = append(files, engineFiles...)

	for _, f := range files {
		if err := fs.WriteFile(f.Path, []byte(f.Content)); err != nil {
			return err
		}

		fmt.Printf("docgen: wrote %s\n", f.Path)
	}

	return nil
}

func read(fs fsadapter.FS, path string, into any) error {
	raw, err := fs.ReadFile(path)
	if err != nil {
		return err
	}

	if err := yaml.UnmarshalStrict(raw, into); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	return nil
}

// engineDocs renders usage and schema for every engine, from its forge-dev.yaml
// and the OpenAPI document it generates its types from.
func engineDocs(fs fsadapter.FS) ([]docstypes.File, error) {
	dirs, err := filepath.Glob(filepath.Join("cmd", "factory-lang-*"))
	if err != nil {
		return nil, fmt.Errorf("looking for engines: %w", err)
	}

	if len(dirs) == 0 {
		return nil, fmt.Errorf("no engine found under cmd/factory-lang-*")
	}

	var out []docstypes.File

	for _, dir := range dirs {
		var engine docstypes.Engine

		if err := read(fs, filepath.Join(dir, "forge-dev.yaml"), &engine); err != nil {
			return nil, err
		}

		schemas, err := specadapter.Read(fs, filepath.Join(dir, engine.OpenAPI.SpecPath))
		if err != nil {
			return nil, err
		}

		usage, err := docscontroller.RenderUsage(dir, engine)
		if err != nil {
			return nil, err
		}

		schema, err := docscontroller.RenderSchema(dir, engine, schemas)
		if err != nil {
			return nil, err
		}

		out = append(out, usage, schema)
	}

	return out, nil
}
