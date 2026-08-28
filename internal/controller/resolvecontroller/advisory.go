package resolvecontroller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	spec "github.com/alexandremahdhaoui/forge-register-spec/pkg/registertypes"

	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

// advisoryGate decides what a finding does to a resolution.
//
// A finding blocks. Nothing else does. A package the feed has never heard of,
// or one it could not be asked about, warns and carries its reason - refusing
// to build because a feed was down protects nobody, and the previous
// behaviour of recording those as clean is what let 56 packages go unchecked
// without anyone noticing.
//
// The way past a finding is to name it. That is a decision somebody makes on
// purpose and a reviewer can see, and naming one id does not accept the next.
type advisoryGate struct {
	language string
	name     string
	track    spec.Track
	acks     []config.Acknowledgement
	now      time.Time

	// modules are the member repos that receive this dependency, and reach
	// asks whether any of their builds contains the vulnerable code. Both
	// are optional: with no engine provisioned the error carries one line
	// fewer.
	modules []string
	reach   reachability
	ctx     context.Context //nolint:containedctx // the gate is built per resolution
}

// decide answers the notes a resolution carries and, when it must stop, why.
func (g advisoryGate) decide() ([]string, error) {
	var notes []string

	// Nothing was measured. Say which kind of nothing, and continue.
	if note := g.unmeasuredNote(); note != "" {
		notes = append(notes, note)
	}

	// A record can be wrong in ways the schema forbids and nothing here
	// checked: the quiet way past this gate was to delete the advisory block
	// while leaving outcome: findings and a non-zero vector behind. Removing
	// the whole track file fails loud; removing the block did not.
	if err := g.contradiction(); err != nil {
		return notes, err
	}

	if g.track.Advisory == nil {
		return append(notes, g.deadAcknowledgements()...), nil
	}

	accepted := map[string]config.Acknowledgement{}

	for _, ack := range g.acks {
		accepted[ack.ID] = ack
	}

	var unacknowledged, expired []string

	for _, id := range g.track.Advisory.VulnIds {
		ack, named := accepted[id]
		if !named {
			unacknowledged = append(unacknowledged, id)

			continue
		}

		if ack.Expires == "" {
			continue
		}

		at, err := time.Parse("2006-01-02", ack.Expires)
		if err != nil {
			// A date nobody can read is not a date. Treating it as "never
			// expires" made a typo into a permanent acceptance, silently -
			// which is the opposite of what an expiry is for.
			expired = append(expired,
				fmt.Sprintf("%s (expires %q is not a date, YYYY-MM-DD)", id, ack.Expires))

			continue
		}

		// The named date is included. "accepted until 2026-08-21" plainly
		// means through that day, and time.Parse gives midnight, so the
		// acknowledgement used to die a day before the date it named.
		if !g.now.After(at.AddDate(0, 0, 1)) {
			continue
		}

		expired = append(expired, fmt.Sprintf("%s (accepted until %s)", id, ack.Expires))
	}

	sort.Strings(unacknowledged)
	sort.Strings(expired)

	if len(unacknowledged) == 0 && len(expired) == 0 {
		return append(notes, g.deadAcknowledgements()...), nil
	}

	return notes, fmt.Errorf("%w\n%s", ErrAdvisory, g.report(unacknowledged, expired))
}

// contradiction refuses a record whose own fields disagree. The register
// writes these files and its schema forbids each of these shapes, but a
// consumer that trusts a shared file to be well formed is a consumer that
// can be walked past by editing it.
func (g advisoryGate) contradiction() error {
	counted := g.track.Vulns.Critical + g.track.Vulns.High +
		g.track.Vulns.Medium + g.track.Vulns.Low

	switch {
	case g.track.Outcome == spec.Findings && g.track.Advisory == nil:
		return fmt.Errorf("%w: %s:%s %s records findings and names none - "+
			"the register record is malformed, re-run its pipeline",
			ErrAdvisory, g.language, g.name, g.track.Current)

	case g.track.Outcome == spec.Findings && counted == 0:
		return fmt.Errorf("%w: %s:%s %s records findings and counts none - "+
			"the register record is malformed, re-run its pipeline",
			ErrAdvisory, g.language, g.name, g.track.Current)

	case g.track.Advisory != nil && len(g.track.Advisory.VulnIds) == 0:
		return fmt.Errorf("%w: %s:%s %s carries an advisory naming no "+
			"vulnerability - there is nothing to acknowledge and nothing to "+
			"fix, so the register record is malformed",
			ErrAdvisory, g.language, g.name, g.track.Current)

	case g.track.Outcome == spec.Clean && counted > 0:
		return fmt.Errorf("%w: %s:%s %s records clean and counts %d "+
			"vulnerabilities - the register record is malformed",
			ErrAdvisory, g.language, g.name, g.track.Current, counted)
	}

	return nil
}

// unmeasuredNote says, in words, why a package carries no findings when
// nothing was actually asked about it.
func (g advisoryGate) unmeasuredNote() string {
	switch g.track.Outcome {
	case spec.NotFound, spec.Unreachable:
		reason := ""
		if g.track.Reason != nil {
			reason = *g.track.Reason
		}

		// One tight line. The register's own reason is a sentence of its own
		// and repeating it inside a second sentence produced a paragraph per
		// package - 56 of them on this workspace, which nobody reads, which
		// is the same failure this note exists to prevent.
		return fmt.Sprintf("%s:%s %s is unexamined, not known to be safe. %s",
			g.language, g.name, g.track.Current, sentence(reason))
	}

	return ""
}

// deadAcknowledgements names an acceptance that has outlived its finding, the
// same way a dead soft pin is named. An accepted risk that no longer exists
// is a line nobody will remove unless something says so.
func (g advisoryGate) deadAcknowledgements() []string {
	live := map[string]bool{}

	if g.track.Advisory != nil {
		for _, id := range g.track.Advisory.VulnIds {
			live[id] = true
		}
	}

	var notes []string

	for _, ack := range g.acks {
		if !live[ack.ID] {
			notes = append(notes, fmt.Sprintf(
				"%s:%s acknowledges %s and the register no longer reports it on %s - "+
					"remove the acknowledgement",
				g.language, g.name, ack.ID, g.track.Current))
		}
	}

	sort.Strings(notes)

	return notes
}

// report is the whole answer, in the order a person acts on it: what is
// wrong, then the ways out, best first.
func (g advisoryGate) report(unacknowledged, expired []string) string {
	adv := g.track.Advisory

	var b strings.Builder

	total := len(unacknowledged) + len(expired)

	fmt.Fprintf(&b, "%s:%s %s has %d unacknowledged finding(s). "+
		"A pin cannot silence this.\n\n",
		g.language, g.name, g.track.Current, total)

	// Asked only here, on the path that is already about to fail. A green
	// resolution never pays for it.
	reached := g.reach.lines(g.ctx, g.modules, unacknowledged, deref(adv.AffectedImports))

	for _, id := range unacknowledged {
		fmt.Fprintf(&b, "  %s\n", id)

		if line := reached[id]; line != "" {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}

	for _, id := range expired {
		fmt.Fprintf(&b, "  %s - the acknowledgement expired\n", id)
	}

	fmt.Fprintf(&b, "\n  severity  : %s\n", severityWord(string(adv.Severity)))
	fmt.Fprintf(&b, "  published : %s\n", adv.Since.Format("2006-01-02"))

	if imports := deref(adv.AffectedImports); len(imports) > 0 {
		fmt.Fprintf(&b, "  affects   : %s\n", strings.Join(imports, ", "))
	} else {
		fmt.Fprintf(&b, "  affects   : the feed publishes no import scope, so assume all of it\n")
	}

	b.WriteString("\nHow to proceed, best first:\n\n")

	// The fix comes from the record, never from a sentence we wrote. This
	// line used to read "no fix upstream" whether or not anyone had looked.
	if fixes := deref(adv.FixedIn); len(fixes) > 0 {
		fmt.Fprintf(&b, "  1. upgrade. The feed names %s as fixing this. If the register is\n"+
			"     behind, run its pipeline; if a pin is holding you here, raise it.\n",
			strings.Join(fixes, " or "))
	} else {
		b.WriteString("  1. no newer version fixes this. The feed names none.\n")
	}

	b.WriteString("  2. use a different package, or a different way of doing this.\n")
	b.WriteString("  3. accept the risk, on purpose, in forge-factory.yaml:\n\n")

	fmt.Fprintf(&b, "       %s:\n         %s:\n           acknowledge:\n", g.language, g.name)

	for _, id := range append(append([]string{}, unacknowledged...), stripNotes(expired)...) {
		fmt.Fprintf(&b, "             - id: %s\n               reason: \"...\"\n", id)
	}

	b.WriteString("\n     A reason is required. Acknowledging one id does not accept the\n")
	b.WriteString("     next one: a new finding blocks again.")

	return b.String()
}

// sentence trims a reason to one clause and punctuates it once. The register
// writes a full sentence; two of them run together read as a stutter.
func sentence(reason string) string {
	if reason == "" {
		return "The register does not say why."
	}

	if i := strings.Index(reason, ". "); i > 0 {
		reason = reason[:i]
	}

	reason = strings.TrimSpace(strings.TrimRight(reason, ". "))

	// A reason that was only punctuation and spaces is empty by now. Slicing
	// it crashed the whole sync with a stack trace; and slicing a byte off a
	// multi-byte first rune corrupted it. Neither is a thing to do to
	// somebody's build because a register record was sloppy.
	if reason == "" {
		return "The register does not say why."
	}

	first, size := utf8.DecodeRuneInString(reason)

	return string(unicode.ToUpper(first)) + reason[size:] + "."
}

func severityWord(s string) string {
	if s == "" || s == "unknown" {
		return "not published by the feed"
	}

	return s
}

func deref(in *[]string) []string {
	if in == nil {
		return nil
	}

	return *in
}

// stripNotes takes the ids back out of the expired lines, which carry their
// dates for the human but must not carry them into the yaml.
func stripNotes(in []string) []string {
	out := make([]string, 0, len(in))

	for _, line := range in {
		id, _, _ := strings.Cut(line, " ")
		out = append(out, id)
	}

	return out
}
