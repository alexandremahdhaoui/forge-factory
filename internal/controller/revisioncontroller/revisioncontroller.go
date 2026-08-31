package revisioncontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
	"github.com/alexandremahdhaoui/forge-revision-spec/pkg/revisiontypes"
)

const (
	ToolGet  = "get"
	ToolList = "list"

	KindRevision = "revision"

	// KindDependencyLock is the record family forge-ci mints alongside a
	// revision: one record per lockfile, keyed <revision>/<path>, carrying
	// the content and its sha256. Checkout restores them so a frozen build
	// of the checked-out revision verifies against the closure it was
	// actually proven with.
	KindDependencyLock = "dependency-lock"
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

// The wire type is generated from forge-revision-spec's schema, so a change to
// the contract is a compile error here rather than a silent misread. It is
// mapped into the internal shape at this boundary and goes no further.

// listOutput is the list tool's answer: keys relative to the asked-for
// prefix, slash-separated.
type listOutput struct {
	Keys []string `json:"keys"`
}

// lockRecord mirrors what forge-ci stores per lockfile. The content rides in
// a string - a byte array marshals to base64 and the generated MCP schema
// refuses it.
type lockRecord struct {
	Revision string `json:"revision"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Lockfile string `json:"lockfile"`
}

// Result is what a checkout did, so a caller can print it.
type Result struct {
	Revision string            `json:"revision"`
	Repos    map[string]string `json:"repos"`

	// Locks are the lockfile paths restored from the revision's
	// dependency-lock records, root-relative and sorted. Empty for a
	// revision minted before lock recording existed.
	Locks []string `json:"locks,omitempty"`
}

// Reviser is what a driver accepts, declared in the package that implements it.
type Reviser interface {
	Checkout(ctx context.Context, f config.Factory, root, id string) (Result, error)
}

type Controller struct {
	caller engineadapter.Caller
	fs     fsadapter.FS
	git    gitadapter.Git
}

var _ Reviser = (*Controller)(nil)

func New(caller engineadapter.Caller, fs fsadapter.FS, git gitadapter.Git) *Controller {
	return &Controller{caller: caller, fs: fs, git: git}
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

	var wire revisiontypes.Revision

	if err := json.Unmarshal([]byte(payload), &wire); err != nil {
		return Revision{}, fmt.Errorf("the payload is not a revision: %w", err)
	}

	if wire.Id != "" {
		rev.ID = wire.Id
	}

	if wire.Repos != nil {
		for name, sha := range *wire.Repos {
			rev.Repos[name] = sha
		}
	}

	if wire.Dirty != nil {
		rev.Dirty = append(rev.Dirty, *wire.Dirty...)
	}
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

	locks, err := c.restoreLocks(ctx, f, root, rev.ID)
	if err != nil {
		return Result{}, err
	}

	result.Locks = locks

	return result, nil
}

// restoreLocks writes the revision's dependency-lock records back into the
// workspace, so the checked-out tree carries the exact closure the revision
// was proven with. A revision minted before lock recording existed has no
// records, which is an empty list, not an error.
func (c *Controller) restoreLocks(
	ctx context.Context,
	f config.Factory,
	root, id string,
) ([]string, error) {
	spec := specWithLockKind(f.State.Spec)

	var listed listOutput

	in := getInput{Kind: KindDependencyLock, Key: id, Spec: spec}

	if err := c.caller.Call(ctx, f.State.Engine, ToolList, in, &listed); err != nil {
		return nil, fmt.Errorf("listing dependency locks for %q: %w", id, err)
	}

	sort.Strings(listed.Keys)

	locks := make([]string, 0, len(listed.Keys))

	for _, key := range listed.Keys {
		var out getOutput

		full := getInput{Kind: KindDependencyLock, Key: id + "/" + key, Spec: spec}

		if err := c.caller.Call(ctx, f.State.Engine, ToolGet, full, &out); err != nil {
			return nil, fmt.Errorf("reading dependency lock %q: %w", key, err)
		}

		// The list just named it, so an absent record is a store that
		// changed under the read, never an ordinary miss.
		if !out.Found {
			return nil, fmt.Errorf("reading dependency lock %q: the store listed it and then did not hold it", key)
		}

		var record lockRecord
		if err := json.Unmarshal([]byte(out.Payload), &record); err != nil {
			return nil, fmt.Errorf("reading dependency lock %q: the payload is not a lock record: %w", key, err)
		}

		sum := sha256.Sum256([]byte(record.Lockfile))
		if got := hex.EncodeToString(sum[:]); got != record.SHA256 {
			return nil, fmt.Errorf(
				"restoring dependency lock %s: the content does not hash to the recorded sha256 "+
					"(record says %s, the content hashes to %s)", record.Path, record.SHA256, got)
		}

		rel := filepath.Clean(filepath.FromSlash(record.Path))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("restoring dependency lock %s: the path escapes the workspace", record.Path)
		}

		target := filepath.Join(root, rel)

		if err := c.fs.MkdirAll(filepath.Dir(target)); err != nil {
			return nil, fmt.Errorf("restoring dependency lock %s: %w", record.Path, err)
		}

		if err := c.fs.WriteFile(target, []byte(record.Lockfile)); err != nil {
			return nil, fmt.Errorf("restoring dependency lock %s: %w", record.Path, err)
		}

		locks = append(locks, record.Path)
	}

	sort.Strings(locks)

	return locks, nil
}

// specWithLockKind hands the state engine a spec that names the
// dependency-lock kind. The kind list is per-request caller configuration on
// a conforming store, so asking for it is always safe: a store that never
// recorded a lock answers an empty list. Without this, a checkout against a
// pipeline that predates lock recording would fail on an unknown kind
// instead of restoring nothing.
func specWithLockKind(spec map[string]any) map[string]any {
	out := make(map[string]any, len(spec)+1)
	for k, v := range spec {
		out[k] = v
	}

	kinds, _ := out["kinds"].([]any)
	for _, e := range kinds {
		if name, ok := e.(string); ok && name == KindDependencyLock {
			return out
		}
	}

	out["kinds"] = append(append([]any{}, kinds...), KindDependencyLock)

	return out
}
