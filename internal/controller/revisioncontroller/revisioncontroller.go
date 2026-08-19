package revisioncontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

const (
	ToolGet = "get"

	KindRevision = "revision"
)

var (
	ErrNoState  = errors.New("the factory declares no state engine")
	ErrNotFound = errors.New("no such revision")
	ErrMissing  = errors.New("a repo the revision names is not checked out")
)

// Revision is the internal shape. It matches forge-revision-spec's schema and
// is decoded from the engine's wire types at this boundary, so the engine's
// choices never reach the controllers.
type Revision struct {
	ID    string
	Repos map[string]string
	Dirty []string
}

type getInput struct {
	Kind string         `json:"kind"`
	Key  string         `json:"key"`
	Spec map[string]any `json:"spec"`
}

type getOutput struct {
	Found bool `json:"found"`

	// The contract carries a payload as a JSON document in a string, not as an
	// object. Reading it as a map fails against every conforming engine.
	Payload string `json:"payload,omitempty"`
}

// revisionWire is the Revision schema from forge-revision-spec. It is mapped
// into the internal shape at this boundary.
type revisionWire struct {
	ID    string            `json:"id"`
	Repos map[string]string `json:"repos"`
	Dirty []string          `json:"dirty"`
}

// Result is what a checkout did, so a caller can print it.
type Result struct {
	Revision string            `json:"revision"`
	Repos    map[string]string `json:"repos"`
}

// Reviser is what a driver accepts, declared in the package that implements it.
type Reviser interface {
	Checkout(ctx context.Context, f config.Factory, root, id string) (Result, error)
}

type Controller struct {
	caller engineadapter.Caller
	git    gitadapter.Git
}

var _ Reviser = (*Controller)(nil)

func New(caller engineadapter.Caller, git gitadapter.Git) *Controller {
	return &Controller{caller: caller, git: git}
}

// Get reads one revision through the state engine. forge-factory speaks the
// same transport as forge-ci and imports none of it.
func (c *Controller) Get(ctx context.Context, f config.Factory, id string) (Revision, error) {
	if f.State == nil {
		return Revision{}, ErrNoState
	}

	spec := f.State.Spec
	if spec == nil {
		spec = map[string]any{}
	}

	var out getOutput

	in := getInput{Kind: KindRevision, Key: id, Spec: spec}

	if err := c.caller.Call(ctx, f.State.Engine, ToolGet, in, &out); err != nil {
		return Revision{}, fmt.Errorf("reading revision %q: %w", id, err)
	}

	if !out.Found {
		return Revision{}, fmt.Errorf("reading revision %q: %w", id, ErrNotFound)
	}

	rev, err := decode(id, out.Payload)
	if err != nil {
		return Revision{}, fmt.Errorf("reading revision %q: %w", id, err)
	}

	return rev, nil
}

func decode(id, payload string) (Revision, error) {
	rev := Revision{ID: id, Repos: map[string]string{}}

	if payload == "" {
		return rev, nil
	}

	var wire revisionWire

	if err := json.Unmarshal([]byte(payload), &wire); err != nil {
		return Revision{}, fmt.Errorf("the payload is not a revision: %w", err)
	}

	if wire.ID != "" {
		rev.ID = wire.ID
	}

	for name, sha := range wire.Repos {
		rev.Repos[name] = sha
	}

	rev.Dirty = append(rev.Dirty, wire.Dirty...)
	sort.Strings(rev.Dirty)

	return rev, nil
}

// Checkout puts every member on the SHA the revision proved. A repo the
// revision does not name is left alone, because the revision says nothing
// about it.
func (c *Controller) Checkout(ctx context.Context, f config.Factory, root, id string) (Result, error) {
	rev, err := c.Get(ctx, f, id)
	if err != nil {
		return Result{}, err
	}

	result := Result{Revision: rev.ID, Repos: map[string]string{}}

	for _, repo := range f.Repos {
		sha, ok := rev.Repos[repo.Name]
		if !ok {
			continue
		}

		dir := filepath.Join(root, repo.Name)

		isRepo, err := c.git.IsRepo(ctx, dir)
		if err != nil {
			return Result{}, err
		}

		if !isRepo {
			return Result{}, fmt.Errorf("%w: %s", ErrMissing, repo.Name)
		}

		if err := c.git.Checkout(ctx, dir, sha); err != nil {
			return Result{}, err
		}

		result.Repos[repo.Name] = sha
	}

	return result, nil
}
