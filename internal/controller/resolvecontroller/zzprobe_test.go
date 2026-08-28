package resolvecontroller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

func advTrack(ids []string, imports []string) string {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"advisory": map[string]any{
			"vulnIds": ids, "severity": "high", "since": now,
			"affectedImports": imports,
		},
	})
	return string(raw)
}

// PROBE 1: what dir does the gate hand the engine?
func TestProbeDir(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": advTrack([]string{"CVE-1"}, []string{"example.com/pkg/inner"})})
	var seen string
	opt := resolvecontroller.WithReachability(func(_ context.Context, dir string, _ []byte) ([]byte, error) {
		seen = dir
		return []byte(`{"depth":"imports","answers":[]}`), nil
	})
	_, _, _ = newController(opt).Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	fmt.Printf("PROBE dir seen = %q\n  root = %q\n", seen, root)
}

// PROBE 2: expiry boundary. now is 2026-08-21T12:00Z.
func TestProbeExpiryBoundary(t *testing.T) {
	for _, exp := range []string{"2026-08-20", "2026-08-21", "2026-08-22", "not-a-date", ""} {
		f, root := register(t, map[string]string{"go/example.com/pkg/1": advTrack([]string{"CVE-1"}, nil)})
		_, _, err := newController().Resolve(context.Background(), f, root, "go",
			map[string]config.DependencySpec{"example.com/pkg": {Track: "1",
				Acknowledge: []config.Acknowledgement{{ID: "CVE-1", Reason: "r", Expires: exp}}}})
		fmt.Printf("PROBE expires=%-12q blocked=%v\n", exp, err != nil)
	}
}

// PROBE 3: nil Advisory pointer but outcome findings; empty VulnIds.
func TestProbeEmptyVulnIds(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"outcome": "findings",
		"vulns":   map[string]int{"critical": 1, "high": 0, "medium": 0, "low": 0},
		"advisory": map[string]any{
			"vulnIds": []string{}, "severity": "critical", "since": now,
		},
	})
	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})
	v, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	fmt.Printf("PROBE emptyVulnIds: version=%q notes=%v err=%v\n", v, notes, err)
}

// PROBE 4: outcome findings with NO advisory object at all.
func TestProbeNoAdvisoryObject(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"outcome": "findings",
		"vulns":   map[string]int{"critical": 2, "high": 0, "medium": 0, "low": 0},
	})
	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})
	v, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	fmt.Printf("PROBE noAdvisoryObject: version=%q notes=%v err=%v\n", v, notes, err)
}

// PROBE 5: does ResolveTool allow acknowledgement at all?
func TestProbeResolveTool(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": advTrack([]string{"CVE-1"}, nil)})
	_, _, err := newController().ResolveTool(context.Background(), f, root, "go:example.com/pkg")
	fmt.Printf("PROBE resolveTool err = %v\n", err != nil)
	require.Error(t, err)
}
