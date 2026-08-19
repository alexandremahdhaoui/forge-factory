package statuscontroller

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

// RepoStatus is one member measured against the spec.
type RepoStatus struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Cloned  bool   `json:"cloned"`
	Dirty   bool   `json:"dirty"`
	Head    string `json:"head,omitempty"`
}

// ModuleStatus is one shared spec measured against what it is pinned at.
type ModuleStatus struct {
	Path   string `json:"path"`
	Pinned string `json:"pinned"`
	Latest string `json:"latest"`
}

// Behind is true when the local checkout carries a newer tag than the pin. The
// pin is what a build without that checkout fetches, so a stale one hands a
// lone builder a spec the workspace stopped using.
func (m ModuleStatus) Behind() bool {
	return m.Pinned != "" && m.Latest != "" && m.Pinned != m.Latest
}

// Report answers what on disk disagrees with the spec.
type Report struct {
	Root    string         `json:"root"`
	Repos   []RepoStatus   `json:"repos"`
	Unknown []string       `json:"unknown"`
	Modules []ModuleStatus `json:"modules"`
}

// Agrees is true when every member is cloned and nothing on disk is unclaimed.
func (r Report) Agrees() bool {
	if len(r.Unknown) > 0 {
		return false
	}

	for _, repo := range r.Repos {
		if !repo.Cloned {
			return false
		}
	}

	for _, m := range r.Modules {
		if m.Behind() {
			return false
		}
	}

	return true
}

// Stater is what a driver accepts, declared in the package that implements it.
type Stater interface {
	Status(ctx context.Context, f config.Factory, root string) (Report, error)
}

type Controller struct {
	fs  fsadapter.FS
	git gitadapter.Git
}

var _ Stater = (*Controller)(nil)

func New(fs fsadapter.FS, git gitadapter.Git) *Controller {
	return &Controller{fs: fs, git: git}
}

// isRepo answers for a path that may be a plain file. git refuses to run inside
// one, and the workspace root holds the factory file itself.
func (c *Controller) isRepo(ctx context.Context, dir string) (bool, error) {
	isDir, err := c.fs.IsDir(dir)
	if err != nil || !isDir {
		return false, err
	}

	return c.git.IsRepo(ctx, dir)
}

func (c *Controller) Status(ctx context.Context, f config.Factory, root string) (Report, error) {
	report := Report{Root: root, Repos: []RepoStatus{}, Unknown: []string{}}

	claimed := map[string]bool{}

	for _, repo := range f.Repos {
		claimed[repo.Name] = true

		dir := filepath.Join(root, repo.Name)

		status := RepoStatus{Name: repo.Name}

		present, err := c.fs.Exists(dir)
		if err != nil {
			return Report{}, err
		}

		status.Present = present

		if present {
			isRepo, err := c.isRepo(ctx, dir)
			if err != nil {
				return Report{}, err
			}

			status.Cloned = isRepo
		}

		if status.Cloned {
			dirty, err := c.git.Dirty(ctx, dir)
			if err != nil {
				return Report{}, err
			}

			status.Dirty = dirty

			head, err := c.git.HeadSHA(ctx, dir)
			if err == nil {
				status.Head = head
			}
		}

		report.Repos = append(report.Repos, status)
	}

	entries, err := c.fs.List(root)
	if err != nil {
		return Report{}, err
	}

	for _, name := range entries {
		if claimed[name] {
			continue
		}

		isRepo, err := c.isRepo(ctx, filepath.Join(root, name))
		if err != nil {
			return Report{}, err
		}

		if isRepo {
			report.Unknown = append(report.Unknown, name)
		}
	}

	sort.Strings(report.Unknown)

	modules, err := c.modules(ctx, f, root)
	if err != nil {
		return Report{}, err
	}

	report.Modules = modules

	return report, nil
}

// modules compares each pinned version against the tag its local checkout
// carries. Nothing else notices when the two drift apart, because the local
// checkout always wins and the pin is only read by a builder that lacks it.
func (c *Controller) modules(
	ctx context.Context,
	f config.Factory,
	root string,
) ([]ModuleStatus, error) {
	out := []ModuleStatus{}

	for _, path := range f.ModulePaths() {
		m := f.Modules[path]

		status := ModuleStatus{Path: path, Pinned: m.Version}

		if m.Path != "" {
			dir := filepath.Join(root, m.Path)

			isRepo, err := c.isRepo(ctx, dir)
			if err != nil {
				return nil, err
			}

			if isRepo {
				latest, err := c.git.LatestTag(ctx, dir)
				if err != nil {
					return nil, err
				}

				status.Latest = latest
			}
		}

		out = append(out, status)
	}

	return out, nil
}
