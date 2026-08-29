package resolvecontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
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
	run func(ctx context.Context, dirs []string, stdin []byte) ([]byte, error)
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
func (r reachability) lines(ctx context.Context, dirs, ids, imports []string) map[string]string {
	// No modules to read, no scope to check against, or no engine: no line.
	// Never a line saying not-reached, which would be a clear granted on the
	// strength of having looked at nothing.
	if len(dirs) == 0 || len(ids) == 0 || len(imports) == 0 || r.run == nil {
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

	raw, err := r.run(ctx, dirs, payload)
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
func RunVulncheckEngine(ctx context.Context, dirs []string, stdin []byte) ([]byte, error) {
	// A bounded wait. The engine is an enrichment on a path that is already
	// failing; a hung one must not hang the sync, and there is nothing here
	// worth waiting minutes for.
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()

	args := []string{
		"run", "github.com/alexandremahdhaoui/forge-go-vulncheck",
		"go-vulncheck", "--",
	}
	for _, d := range dirs {
		args = append(args, "--dir", d)
	}

	cmd := exec.CommandContext(ctx, "forge", args...)
	cmd.Stdin = bytes.NewReader(stdin)

	// A bounded read. A runaway engine should be an error, not an OOM.
	var out bytes.Buffer

	cmd.Stdout = &limitedWriter{w: &out, left: maxEngineOutput}

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running the reachability engine: %w", err)
	}

	return out.Bytes(), nil
}

const (
	// engineTimeout bounds the wait. Reading a package graph is seconds.
	engineTimeout = 90 * time.Second
	// maxEngineOutput bounds the read. A report is kilobytes.
	maxEngineOutput = 4 << 20
)

// limitedWriter stops after n bytes and says so, rather than growing until
// the machine gives up.
type limitedWriter struct {
	w    *bytes.Buffer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.left {
		return 0, fmt.Errorf("the reachability engine wrote more than %d bytes", maxEngineOutput)
	}

	l.left -= len(p)

	return l.w.Write(p)
}
