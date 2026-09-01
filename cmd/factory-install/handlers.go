package main

import (
	"context"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/installcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
)

// NewHandlers wires the installer into the generated tool surface.
func NewHandlers() Handlers {
	ctrl := installcontroller.New()

	return Handlers{
		Install: func(_ context.Context, in InstallInput) (*InstallOutput, error) {
			archives := make([]installcontroller.Fetched, 0, len(in.Archives))
			for _, a := range in.Archives {
				archives = append(archives, installcontroller.Fetched{
					Artifact: toArtifact(a.Artifact),
					Path:     a.Path,
				})
			}

			installed, err := ctrl.Install(archives, in.Prefix)
			if err != nil {
				return nil, err
			}

			return &InstallOutput{Installed: installed}, nil
		},
	}
}

func toArtifact(a RuntimeArtifact) runtimetypes.Artifact {
	picks := make([]runtimetypes.Pick, 0, len(a.Picks))
	for _, p := range a.Picks {
		picks = append(picks, runtimetypes.Pick{From: p.From, At: p.At})
	}

	return runtimetypes.Artifact{
		URL: a.Url, SHA256: a.Sha256, Unpack: a.Unpack, Strip: a.Strip, Picks: picks,
	}
}
