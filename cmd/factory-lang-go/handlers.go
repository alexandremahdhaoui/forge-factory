package main

import (
	"context"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/rendercontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/rendertypes"
)

// NewHandlers wires one renderer into the generated tool surface. The generated
// types are the wire, so they are mapped here rather than reaching the
// controller.
func NewHandlers() Handlers {
	renderer := rendercontroller.Go{}

	return Handlers{
		Language: func(_ context.Context, _ RenderInput) (*LanguageOutput, error) {
			return &LanguageOutput{Language: renderer.Language()}, nil
		},
		Render: func(_ context.Context, in RenderInput) (*RenderOutput, error) {
			files, err := renderer.Render(toInput(in))
			if err != nil {
				return nil, err
			}

			return &RenderOutput{Files: fromFiles(files)}, nil
		},
	}
}

func toInput(in RenderInput) rendertypes.Input {
	repos := make([]rendertypes.Repo, 0, len(in.Repos))

	for _, r := range in.Repos {
		repos = append(repos, rendertypes.Repo{
			Name:      r.Name,
			Path:      r.Path,
			Languages: r.Languages,
			Identity:  r.Identity,
		})
	}

	return rendertypes.Input{Root: in.Root, Repos: repos, Dependencies: in.Dependencies}
}

func fromFiles(files []rendertypes.File) []File {
	out := make([]File, 0, len(files))

	for _, f := range files {
		out = append(out, File{Path: f.Path, Content: f.Content, Gitignore: f.Gitignore})
	}

	return out
}
