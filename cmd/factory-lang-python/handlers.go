package main

import (
	"context"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/describecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/rendercontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/rendertypes"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
)

// NewHandlers wires one renderer into the generated tool surface. The generated
// types are the wire, so they are mapped here rather than reaching the
// controller.
func NewHandlers() Handlers {
	renderer := rendercontroller.Python{}

	return Handlers{
		Describe: describeHandler(),
		Language: func(_ context.Context, _ RenderInput) (*LanguageOutput, error) {
			return &LanguageOutput{Language: renderer.Language()}, nil
		},
		Render: func(_ context.Context, in RenderInput) (*RenderOutput, error) {
			out, err := renderer.Render(toInput(in))
			if err != nil {
				return nil, err
			}

			return &RenderOutput{
				Files:          fromFiles(out.Files),
				DependencyLock: fromCommands(out.DependencyLock),
				Lockfiles:      out.Lockfiles,
			}, nil
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

	return rendertypes.Input{
		Root:         in.Root,
		Repos:        repos,
		Dependencies: in.Dependencies,
		Dev:          in.DevDependencies,
	}
}

func fromCommands(commands []rendertypes.Command) []Command {
	out := make([]Command, 0, len(commands))

	for _, c := range commands {
		out = append(out, Command{
			Dir:      c.Dir,
			Command:  c.Command,
			Args:     c.Args,
			Env:      c.Env,
			Optional: c.Optional,
		})
	}

	return out
}

func fromFiles(files []rendertypes.File) []File {
	out := make([]File, 0, len(files))

	for _, f := range files {
		out = append(out, File{
			Path:       f.Path,
			Content:    f.Content,
			Gitignore:  f.Gitignore,
			AlsoIgnore: f.AlsoIgnore,
		})
	}

	return out
}

// describeHandler answers the runtime description through the language's
// describer. Declared apart from NewHandlers so the wiring above stays the
// render story it always was.
func describeHandler() func(context.Context, DescribeInput) (*RuntimeDescription, error) {
	describer := describecontroller.Python{}

	return func(_ context.Context, in DescribeInput) (*RuntimeDescription, error) {
		out, err := describer.Describe(runtimetypes.Input{
			Runtime: in.Runtime, Version: in.Version,
			OS: in.Os, Arch: in.Arch, Params: in.Params,
		})
		if err != nil {
			return nil, err
		}

		return fromDescription(out), nil
	}
}

func fromDescription(d runtimetypes.Description) *RuntimeDescription {
	artifacts := make([]RuntimeArtifact, 0, len(d.Artifacts))

	for _, a := range d.Artifacts {
		picks := make([]RuntimePick, 0, len(a.Picks))
		for _, p := range a.Picks {
			picks = append(picks, RuntimePick{From: p.From, At: p.At})
		}

		artifacts = append(artifacts, RuntimeArtifact{
			Url: a.URL, Sha256: a.SHA256, Unpack: a.Unpack, Strip: a.Strip, Picks: picks,
		})
	}

	prereqs := make([]RuntimePrerequisite, 0, len(d.Prerequisites))
	for _, p := range d.Prerequisites {
		prereqs = append(prereqs, RuntimePrerequisite{Name: p.Name, Reason: p.Reason, Verify: p.Verify})
	}

	return &RuntimeDescription{
		Runtime: d.Runtime, Version: d.Version,
		Artifacts: artifacts, Bins: d.Bins, Env: d.Env,
		Prerequisites: prereqs, Provides: d.Provides,
	}
}
