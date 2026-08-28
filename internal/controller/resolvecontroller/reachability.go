package resolvecontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// reachability asks whether the module's build reaches an advisory at all.
//
// It is an enrichment and nothing more. The finding blocks either way, and
// the answer adds a sentence to the error - because a reachability answer is
// a static approximation and an approximation must not be the thing that
// unblocks a release. The decision stays with the person, written down in
// the factory file where a reviewer sees it.
//
// It runs only when a finding already blocks, so a green resolution pays
// nothing for it. When the engine is not provisioned the line is simply
// absent, which is why nothing here fails.
type reachability struct {
	// run executes the engine. A field so a test needs no binary.
	run func(ctx context.Context, dir string, stdin []byte) ([]byte, error)
}

// finding is what the engine is asked about.
type finding struct {
	ID      string   `json:"id"`
	Imports []string `json:"imports"`
}

// report is what it answers.
type report struct {
	Depth   string `json:"depth"`
	Answers []struct {
		ID      string   `json:"id"`
		Verdict string   `json:"verdict"`
		Via     []string `json:"via"`
		Reason  string   `json:"reason"`
	} `json:"answers"`
}

// lines answers one sentence per finding the engine could speak to, or
// nothing at all. Nothing at all is the normal case: the engine may not be
// provisioned, the ecosystem may have no such tool, and most advisories
// publish no import scope for anything to check against.
func (r reachability) lines(ctx context.Context, dir string, ids, imports []string) map[string]string {
	if len(ids) == 0 || len(imports) == 0 || r.run == nil {
		return nil
	}

	// Every id shares the advisory's scope: the register unions the imports
	// across the ids it names, and the consumer's question is the same for
	// each - is any of this compiled in.
	in := struct {
		Findings []finding `json:"findings"`
	}{}

	for _, id := range ids {
		in.Findings = append(in.Findings, finding{ID: id, Imports: imports})
	}

	payload, err := json.Marshal(in)
	if err != nil {
		return nil
	}

	raw, err := r.run(ctx, dir, payload)
	if err != nil {
		return nil
	}

	var got report
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil
	}

	out := map[string]string{}

	for _, a := range got.Answers {
		switch a.Verdict {
		case "not-reached":
			out[a.ID] = "your code does not reach it: " + a.Reason +
				" (checked at " + got.Depth + " depth, which does not make it safe to ignore)"
		case "reached":
			sort.Strings(a.Via)
			out[a.ID] = "your code reaches it, through " + strings.Join(a.Via, ", ")
		}
	}

	return out
}

// RunVulncheckEngine executes the engine through forge, so it resolves the
// same way every other engine does. A missing engine is an error here and an
// absent line in the report.
func RunVulncheckEngine(ctx context.Context, dir string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "forge", "run",
		"github.com/alexandremahdhaoui/forge-go-vulncheck", "go-vulncheck",
		"--", "--dir", dir)
	cmd.Stdin = strings.NewReader(string(stdin))

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running the reachability engine: %w", err)
	}

	return out, nil
}
