package clonecontroller

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

// Report is what a clone did, so a caller can print it and a test can assert it.
type Report struct {
	Root    string   `json:"root"`
	Cloned  []string `json:"cloned"`
	Present []string `json:"present"`
}

// Cloner is what a driver accepts, declared in the package that implements it.
type Cloner interface {
	Clone(ctx context.Context, f config.Factory, root string) (Report, error)
}

type Controller struct {
	fs  fsadapter.FS
	git gitadapter.Git
}

var _ Cloner = (*Controller)(nil)

func New(fs fsadapter.FS, git gitadapter.Git) *Controller {
	return &Controller{fs: fs, git: git}
}

// Clone fetches every member that is not there yet. It is what turns a lone
// checkout of the factory into a workspace, which is the only way anything
// here builds. A member already present is left alone, because a clone must
// never be the thing that discards local work.
func (c *Controller) Clone(ctx context.Context, f config.Factory, root string) (Report, error) {
	report := Report{Root: root, Cloned: []string{}, Present: []string{}}

	for _, repo := range f.Repos {
		dir := filepath.Join(root, repo.Name)

		exists, err := c.fs.Exists(dir)
		if err != nil {
			return Report{}, err
		}

		if exists {
			report.Present = append(report.Present, repo.Name)

			continue
		}

		if err := c.git.Clone(ctx, repo.URL, dir); err != nil {
			return Report{}, fmt.Errorf("cloning %s: %w", repo.Name, err)
		}

		report.Cloned = append(report.Cloned, repo.Name)
	}

	return report, nil
}
