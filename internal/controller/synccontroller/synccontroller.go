package synccontroller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/repoadapter"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

const (
	ToolLanguage = "language"
	ToolRender   = "render"

	gitignoreHeader = "# forge-factory materialises these. A version is written in forge-factory.yaml."
)

var ErrLanguage = errors.New("an engine speaks a different language than its alias claims")

// Report is what a sync did, so a caller can print it and a test can assert it.
type Report struct {
	Root    string   `json:"root"`
	Written []string `json:"written"`
	Ignored []string `json:"ignored"`
}

type repoWire struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Languages []string          `json:"languages"`
	Identity  map[string]string `json:"identity,omitempty"`
}

type renderInput struct {
	Root         string            `json:"root"`
	Repos        []repoWire        `json:"repos"`
	Dependencies map[string]string `json:"dependencies"`
}

type fileWire struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Gitignore string `json:"gitignore,omitempty"`
}

type renderOutput struct {
	Files []fileWire `json:"files"`
}

type languageOutput struct {
	Language string `json:"language"`
}

type Controller struct {
	caller engineadapter.Caller
	fs     fsadapter.FS
	repos  repoadapter.Reader
}

func New(caller engineadapter.Caller, fs fsadapter.FS, repos repoadapter.Reader) *Controller {
	return &Controller{caller: caller, fs: fs, repos: repos}
}

// Sync asks every language engine what to write, writes it, and keeps each
// repo's gitignore in step. A version is written in the factory and nowhere
// else, so everything written here is ignored by git.
func (c *Controller) Sync(ctx context.Context, f config.Factory, root string) (Report, error) {
	resolved, err := c.resolve(f, root)
	if err != nil {
		return Report{}, err
	}

	report := Report{Root: root, Written: []string{}, Ignored: []string{}}

	ignores := map[string][]string{}

	for _, language := range f.Languages() {
		uri, ok := f.EngineFor(language)
		if !ok {
			return Report{}, fmt.Errorf("no engine is registered for %q", language)
		}

		if err := c.checkLanguage(ctx, uri, language, resolved); err != nil {
			return Report{}, err
		}

		in := renderInput{Root: root, Repos: resolved, Dependencies: f.DependenciesFor(language)}

		var out renderOutput

		if err := c.caller.Call(ctx, uri, ToolRender, in, &out); err != nil {
			return Report{}, fmt.Errorf("rendering %s: %w", language, err)
		}

		for _, file := range out.Files {
			if err := c.fs.WriteFile(file.Path, []byte(file.Content)); err != nil {
				return Report{}, fmt.Errorf("writing %s: %w", file.Path, err)
			}

			report.Written = append(report.Written, file.Path)

			if file.Gitignore != "" {
				ignores[file.Gitignore] = append(ignores[file.Gitignore], filepath.Base(file.Path))
			}
		}
	}

	sort.Strings(report.Written)

	for _, repo := range sortedKeys(ignores) {
		path := filepath.Join(root, repo, ".gitignore")

		changed, err := c.ensureIgnored(path, ignores[repo])
		if err != nil {
			return Report{}, err
		}

		if changed {
			report.Ignored = append(report.Ignored, path)
		}
	}

	return report, nil
}

// checkLanguage refuses an engine registered under the wrong alias, because a
// rust engine behind the go alias would silently render the wrong files. It
// sends a whole input rather than an empty one: a nil slice or map travels as
// null and the engine's own schema refuses it.
func (c *Controller) checkLanguage(
	ctx context.Context,
	uri, alias string,
	repos []repoWire,
) error {
	var out languageOutput

	in := renderInput{Repos: repos, Dependencies: map[string]string{}}

	if err := c.caller.Call(ctx, uri, ToolLanguage, in, &out); err != nil {
		return fmt.Errorf("asking %q what language it speaks: %w", alias, err)
	}

	if out.Language != alias {
		return fmt.Errorf("%w: %q is registered as %q but speaks %q",
			ErrLanguage, uri, alias, out.Language)
	}

	return nil
}

func (c *Controller) resolve(f config.Factory, root string) ([]repoWire, error) {
	out := make([]repoWire, 0, len(f.Repos))

	for _, r := range f.Repos {
		path := filepath.Join(root, r.Name)

		identity, err := c.repos.Identity(path)
		if err != nil {
			return nil, err
		}

		out = append(out, repoWire{
			Name:      r.Name,
			Path:      path,
			Languages: r.Languages,
			Identity:  identity,
		})
	}

	return out, nil
}

// ensureIgnored adds each name to a gitignore without disturbing what is
// already there, and reports whether it changed anything.
func (c *Controller) ensureIgnored(path string, names []string) (bool, error) {
	existing := ""

	exists, err := c.fs.Exists(path)
	if err != nil {
		return false, err
	}

	if exists {
		raw, err := c.fs.ReadFile(path)
		if err != nil {
			return false, err
		}

		existing = string(raw)
	}

	present := map[string]bool{}
	for _, line := range strings.Split(existing, "\n") {
		present[strings.TrimSpace(line)] = true
	}

	var missing []string

	for _, name := range names {
		entry := "/" + name

		if !present[entry] {
			missing = append(missing, entry)
			present[entry] = true
		}
	}

	if len(missing) == 0 {
		return false, nil
	}

	sort.Strings(missing)

	var b strings.Builder

	b.WriteString(existing)

	if existing != "" && !strings.HasSuffix(existing, "\n") {
		b.WriteString("\n")
	}

	if !strings.Contains(existing, gitignoreHeader) {
		b.WriteString("\n" + gitignoreHeader + "\n")
	}

	for _, entry := range missing {
		b.WriteString(entry + "\n")
	}

	if err := c.fs.WriteFile(path, []byte(b.String())); err != nil {
		return false, err
	}

	return true, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
