package main

import (
	"context"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/httpadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/fetchcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
)

// NewHandlers wires the fetcher into the generated tool surface. The
// generated types are the wire, so they are mapped here rather than
// reaching the controller.
func NewHandlers() Handlers {
	ctrl := fetchcontroller.New(httpadapter.New(), fsadapter.New())

	return Handlers{
		Fetch: func(ctx context.Context, in FetchInput) (*FetchOutput, error) {
			rules, err := fetchcontroller.ParseRewrites(in.Spec)
			if err != nil {
				return nil, err
			}

			sha, err := ctrl.Fetch(ctx, toArtifact(in.Artifact), in.Dest, rules)
			if err != nil {
				return nil, err
			}

			return &FetchOutput{Path: in.Dest, Sha256: sha}, nil
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
