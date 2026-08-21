package speccontroller

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

var (
	ErrNotFound  = errors.New("no dependency by that name is declared")
	ErrAmbiguous = errors.New("more than one language declares that dependency")
	ErrExists    = errors.New("a repo by that name is already a member")
	ErrNoRepos   = errors.New("the factory declares no repos list to append to")
)

// Edits change the factory in place, line by line, so comments and ordering
// survive. Re-marshalling the parsed document would rewrite the whole file and
// throw both away.
type Edit struct {
	Line int
	Was  string
	Now  string
}

var depLine = regexp.MustCompile(`^(\s+)("?)([^"\s][^:]*?)("?):\s*(.*?)\s*$`)

// Bump rewrites the version of one dependency. A bare name must be declared by
// exactly one language. Prefix it with the language to name it exactly.
func Bump(raw []byte, dep, version string) ([]byte, Edit, error) {
	language, name := split(dep)

	lines := strings.Split(string(raw), "\n")

	var (
		matches []int
		inDeps  bool
		current string
	)

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if !strings.HasPrefix(line, " ") {
			inDeps = trimmed == "dependencies:"
			current = ""

			continue
		}

		if !inDeps {
			continue
		}

		m := depLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		if len(m[1]) <= 2 {
			current = m[3]

			continue
		}

		if m[3] != name {
			continue
		}

		if language != "" && current != language {
			continue
		}

		matches = append(matches, i)
	}

	switch len(matches) {
	case 0:
		return nil, Edit{}, fmt.Errorf("bumping %q: %w", dep, ErrNotFound)
	case 1:
	default:
		return nil, Edit{}, fmt.Errorf("bumping %q: %w", dep, ErrAmbiguous)
	}

	at := matches[0]
	m := depLine.FindStringSubmatch(lines[at])
	was := lines[at]
	now := fmt.Sprintf("%s%s%s%s: %s", m[1], m[2], m[3], m[4], quote(version))

	lines[at] = now

	out := []byte(strings.Join(lines, "\n"))

	if _, err := config.Parse(out); err != nil {
		return nil, Edit{}, fmt.Errorf("bumping %q: %w", dep, err)
	}

	return out, Edit{Line: at + 1, Was: was, Now: now}, nil
}

// PrunePins rewrites named dependencies with their pin fields dropped, keeping
// track and wraps. The deps are language:name pairs, the shape DeadPin
// answers. Entries must be inline objects - the only shape sync writes.
func PrunePins(raw []byte, deps []string) ([]byte, []Edit, error) {
	f, err := config.Parse(raw)
	if err != nil {
		return nil, nil, err
	}

	lines := strings.Split(string(raw), "\n")
	edits := []Edit{}

	for _, dep := range deps {
		language, name := split(dep)

		d, ok := f.Dependencies[language][name]
		if !ok {
			d, ok = f.Dev[language][name]
		}

		if !ok {
			return nil, nil, fmt.Errorf("pruning %q: %w", dep, ErrNotFound)
		}

		d.Pin, d.Mode, d.Reason, d.Expires = "", "", "", ""

		val, err := json.Marshal(d)
		if err != nil {
			return nil, nil, fmt.Errorf("pruning %q: %w", dep, err)
		}

		at := findDep(lines, language, name)
		if at < 0 {
			return nil, nil, fmt.Errorf("pruning %q: %w", dep, ErrNotFound)
		}

		m := depLine.FindStringSubmatch(lines[at])
		was := lines[at]
		now := fmt.Sprintf("%s%s%s%s: %s", m[1], m[2], m[3], m[4], string(val))
		lines[at] = now

		edits = append(edits, Edit{Line: at + 1, Was: was, Now: now})
	}

	out := []byte(strings.Join(lines, "\n"))

	if _, err := config.Parse(out); err != nil {
		return nil, nil, fmt.Errorf("pruning pins: %w", err)
	}

	return out, edits, nil
}

// findDep locates one dependency line by language and name, in either the
// dependencies or the devDependencies section.
func findDep(lines []string, language, name string) int {
	var (
		inDeps  bool
		current string
	)

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if !strings.HasPrefix(line, " ") {
			inDeps = trimmed == "dependencies:" || trimmed == "devDependencies:"
			current = ""

			continue
		}

		if !inDeps {
			continue
		}

		m := depLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		if len(m[1]) <= 2 {
			current = m[3]

			continue
		}

		if m[3] == name && current == language {
			return i
		}
	}

	return -1
}

// Add appends a member to the repos list.
func Add(raw []byte, repo config.Repo) ([]byte, Edit, error) {
	f, err := config.Parse(raw)
	if err != nil {
		return nil, Edit{}, err
	}

	for _, r := range f.Repos {
		if r.Name == repo.Name {
			return nil, Edit{}, fmt.Errorf("adding %q: %w", repo.Name, ErrExists)
		}
	}

	lines := strings.Split(string(raw), "\n")

	at := lastRepoLine(lines)
	if at < 0 {
		return nil, Edit{}, fmt.Errorf("adding %q: %w", repo.Name, ErrNoRepos)
	}

	block := []string{
		fmt.Sprintf("  - name: %s", repo.Name),
		fmt.Sprintf("    url: %s", quote(repo.URL)),
		fmt.Sprintf("    languages: [%s]", strings.Join(repo.Languages, ", ")),
	}

	merged := append([]string{}, lines[:at+1]...)
	merged = append(merged, block...)
	merged = append(merged, lines[at+1:]...)

	out := []byte(strings.Join(merged, "\n"))

	if _, err := config.Parse(out); err != nil {
		return nil, Edit{}, fmt.Errorf("adding %q: %w", repo.Name, err)
	}

	return out, Edit{Line: at + 2, Now: strings.Join(block, "\n")}, nil
}

// lastRepoLine finds the end of the repos list, so an addition lands inside it
// rather than after whatever section follows.
func lastRepoLine(lines []string) int {
	start := -1

	for i, line := range lines {
		if strings.TrimSpace(line) == "repos:" && !strings.HasPrefix(line, " ") {
			start = i

			break
		}
	}

	if start < 0 {
		return -1
	}

	last := start

	for i := start + 1; i < len(lines); i++ {
		line := lines[i]

		if strings.TrimSpace(line) == "" {
			continue
		}

		if !strings.HasPrefix(line, " ") {
			break
		}

		last = i
	}

	return last
}

// Dependencies lists every declared dependency as language and name, sorted, so
// a caller can suggest one when a bump misses.
func Dependencies(f config.Factory) []string {
	out := []string{}

	for language, deps := range f.Dependencies {
		for name := range deps {
			out = append(out, language+":"+name)
		}
	}

	sort.Strings(out)

	return out
}

func split(dep string) (language, name string) {
	if i := strings.Index(dep, ":"); i > 0 {
		return dep[:i], dep[i+1:]
	}

	return "", dep
}

// quote keeps a version yaml reads as a string. A bare 1 or 2 parses as a
// number and every dependency version is a string.
func quote(v string) string {
	if v == "" {
		return `""`
	}

	if strings.ContainsAny(v, `"'`) {
		return v
	}

	if strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		return `"` + v + `"`
	}

	if strings.Count(v, ".") == 1 && strings.IndexFunc(v, func(r rune) bool {
		return r != '.' && (r < '0' || r > '9')
	}) < 0 {
		return `"` + v + `"`
	}

	return v
}
