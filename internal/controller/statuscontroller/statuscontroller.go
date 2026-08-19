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

// Report answers what on disk disagrees with the spec.
type Report struct {
	Root    string       `json:"root"`
	Repos   []RepoStatus `json:"repos"`
	Unknown []string     `json:"unknown"`
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

	return report, nil
}
