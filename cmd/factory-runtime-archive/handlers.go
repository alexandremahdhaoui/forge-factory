package main

import (
	"context"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/describecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
)

// NewHandlers wires the archive describer into the generated tool surface.
func NewHandlers() Handlers {
	describer := describecontroller.Archive{}

	return Handlers{
		Describe: func(_ context.Context, in DescribeInput) (*RuntimeDescription, error) {
			out, err := describer.Describe(runtimetypes.Input{
				Runtime: in.Runtime, Version: in.Version,
				OS: in.Os, Arch: in.Arch, Params: in.Params,
			})
			if err != nil {
				return nil, err
			}

			return fromDescription(out), nil
		},
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
