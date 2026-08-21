// Package resolvecontroller turns dependency entries into concrete versions.
// A legacy string passes through verbatim; a register entry resolves from the
// register checkout - the local checkout wins, like any member. Resolution is
// strict: a missing package or track fails loud and files a request, an
// advisory pierces every pin, and a pin is never silent.
package resolvecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	spec "github.com/alexandremahdhaoui/forge-register-spec/pkg/registertypes"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

var (
	// ErrNoRegister means an entry resolves from the register and the
	// register checkout is nowhere to be found.
	ErrNoRegister = errors.New("the register checkout is missing")
	// ErrUnregistered means the register does not carry the package or track
	// an entry names. A request was filed; the pipeline answers it.
	ErrUnregistered = errors.New("not in the register")
	// ErrAdvisory means a resolved version carries a security advisory. An
	// advisory pierces every pin.
	ErrAdvisory = errors.New("security advisory")
)

// Resolver resolves one language's dependency entries to versions.
type Resolver interface {
	Resolve(ctx context.Context, f config.Factory, root, language string, deps map[string]config.DependencySpec) (map[string]string, []string, error)
}

type Controller struct {
	fs  fsadapter.FS
	now func() time.Time
}

var _ Resolver = (*Controller)(nil)

func New(fs fsadapter.FS, now func() time.Time) *Controller {
	return &Controller{fs: fs, now: now}
}

// Resolve maps entries to versions. The returned notes are diagnostics the
// driver prints: dead soft pins, hard pins standing, deprecated tracks.
func (c *Controller) Resolve(_ context.Context, f config.Factory, root, language string, deps map[string]config.DependencySpec) (map[string]string, []string, error) {
	out := make(map[string]string, len(deps))

	var notes []string

	names := make([]string, 0, len(deps))
	for name := range deps {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		d := deps[name]

		if !d.FromRegister() {
			out[name] = d.Version

			continue
		}

		version, entryNotes, err := c.resolveEntry(f, root, language, name, d)
		if err != nil {
			return nil, notes, err
		}

		notes = append(notes, entryNotes...)
		out[name] = version
	}

	return out, notes, nil
}

func (c *Controller) resolveEntry(f config.Factory, root, language, name string, d config.DependencySpec) (string, []string, error) {
	dir, err := c.registerDir(f, root)
	if err != nil {
		return "", nil, err
	}

	track, err := c.track(dir, root, language, name, d)
	if err != nil {
		return "", nil, err
	}

	var notes []string

	version := track.Current

	if d.Pin != "" {
		version, notes = applyPin(language, name, d, track)
	}

	// An advisory pierces every pin: whatever a pin froze, a track whose
	// current carries an unfixed vulnerability fails the resolution.
	if track.Advisory != nil {
		return "", notes, fmt.Errorf(
			"%s:%s track %s: %w %s (%s) since %s and no fix upstream - a pin cannot silence this",
			language, name, track.Prefix, ErrAdvisory,
			strings.Join(track.Advisory.VulnIds, ", "), track.Advisory.Severity,
			track.Advisory.Since.Format("2006-01-02"))
	}

	if track.Deprecated != nil {
		notes = append(notes, fmt.Sprintf(
			"%s:%s track %s is deprecated (%s) since %s - move to a successor track",
			language, name, track.Prefix, track.Deprecated.Reason,
			track.Deprecated.Since.Format("2006-01-02")))
	}

	if d.Wraps != "" {
		version = fmt.Sprintf(d.Wraps, version)
	}

	return version, notes, nil
}

// applyPin floors the track with a soft pin or freezes it with a hard one,
// and always explains itself.
func applyPin(language, name string, d config.DependencySpec, track spec.Track) (string, []string) {
	where := fmt.Sprintf("%s:%s", language, name)

	if d.Mode == "hard" {
		return d.Pin, []string{fmt.Sprintf(
			"hard pin %s %s (reason: %s) - the register's track %s is at %s; this consumer is frozen, visibly",
			where, d.Pin, d.Reason, track.Prefix, track.Current)}
	}

	if compareVersions(d.Pin, track.Current) > 0 {
		return d.Pin, []string{fmt.Sprintf(
			"soft pin %s %s (reason: %s) is ahead of track %s (%s) - request an upgrade so the pin can retire",
			where, d.Pin, d.Reason, track.Prefix, track.Current)}
	}

	return track.Current, []string{fmt.Sprintf(
		"soft pin %s %s is behind track %s (%s) - the register is newer; remove this pin",
		where, d.Pin, track.Prefix, track.Current)}
}

// registerDir finds the register checkout: an explicit path, or the directory
// the URL names, rooted in the workspace.
func (c *Controller) registerDir(f config.Factory, root string) (string, error) {
	if f.Register == nil {
		return "", fmt.Errorf("%w: no register block", ErrNoRegister)
	}

	name := f.Register.Path
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(f.Register.URL), ".git")
	}

	dir := filepath.Join(root, name)

	ok, err := c.fs.IsDir(dir)
	if err != nil {
		return "", fmt.Errorf("finding the register: %w", err)
	}

	if !ok {
		return "", fmt.Errorf("%w: %s is not checked out; run: forge-factory clone", ErrNoRegister, dir)
	}

	return dir, nil
}

func (c *Controller) track(dir, root, language, name string, d config.DependencySpec) (spec.Track, error) {
	prefix := d.Track
	if prefix == "" {
		var err error

		prefix, err = c.defaultTrack(dir, language, name)
		if err != nil {
			return spec.Track{}, err
		}
	}

	path := filepath.Join(dir, "index", language, filepath.FromSlash(name), prefix+".json")

	exists, err := c.fs.Exists(path)
	if err != nil {
		return spec.Track{}, fmt.Errorf("reading track %s: %w", path, err)
	}

	if !exists {
		key, ferr := c.fileRequest(dir, language, name, d)
		if ferr != nil {
			return spec.Track{}, ferr
		}

		return spec.Track{}, fmt.Errorf(
			"%s:%s track %q is %w; filed %s - the register pipeline answers it, then sync again",
			language, name, prefix, ErrUnregistered, key)
	}

	raw, err := c.fs.ReadFile(path)
	if err != nil {
		return spec.Track{}, fmt.Errorf("reading track %s: %w", path, err)
	}

	var track spec.Track
	if err := json.Unmarshal(raw, &track); err != nil {
		return spec.Track{}, fmt.Errorf("decoding track %s: %w", path, err)
	}

	return track, nil
}

// defaultTrack is the highest prefix the register carries for the package.
func (c *Controller) defaultTrack(dir, language, name string) (string, error) {
	base := filepath.Join(dir, "index", language, filepath.FromSlash(name))

	entries, err := c.fs.List(base)
	if err != nil {
		return "", fmt.Errorf("listing tracks of %s:%s: %w", language, name, err)
	}

	best := ""

	for _, e := range entries {
		prefix := strings.TrimSuffix(e, ".json")
		if prefix == e {
			continue // a directory, not a track file
		}

		if best == "" || compareVersions(prefix, best) > 0 {
			best = prefix
		}
	}

	if best == "" {
		key, ferr := c.fileRequest(dir, language, name, config.DependencySpec{})
		if ferr != nil {
			return "", ferr
		}

		return "", fmt.Errorf(
			"%s:%s is %w; filed %s - the register pipeline answers it, then sync again",
			language, name, ErrUnregistered, key)
	}

	return best, nil
}

// fileRequest writes an admission request into the register checkout. Filing
// is not writing the index: requests are the register's only door, and the
// pipeline answers them.
func (c *Controller) fileRequest(dir, language, name string, d config.DependencySpec) (string, error) {
	now := c.now()

	request := spec.Request{
		Type:      spec.Admission,
		Package:   name,
		Ecosystem: spec.RequestEcosystem(language),
		Reason:    "filed by forge-factory sync: the workspace factory names this package",
		CreatedAt: now,
	}

	if d.Track != "" {
		track := d.Track
		request.Track = &track
	}

	if d.Reason != "" {
		request.Reason = d.Reason
	}

	key := language + "/" + name + "/" + strconv.FormatInt(now.Unix(), 10) + "-admission"

	payload, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encoding the request: %w", err)
	}

	path := filepath.Join(dir, "requests", filepath.FromSlash(key)+".json")
	if err := c.fs.WriteFile(path, payload); err != nil {
		return "", fmt.Errorf("filing the request: %w", err)
	}

	return key, nil
}

// compareVersions orders dotted versions numerically, tolerating a leading v
// and sorting a pre-release below its release. The same ordering the register
// uses.
func compareVersions(a, b string) int {
	ap, apre := parseVersion(a)
	bp, bpre := parseVersion(b)

	for i := 0; i < len(ap) || i < len(bp); i++ {
		av, bv := 0, 0
		if i < len(ap) {
			av = ap[i]
		}

		if i < len(bp) {
			bv = bp[i]
		}

		if av != bv {
			if av < bv {
				return -1
			}

			return 1
		}
	}

	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	case apre < bpre:
		return -1
	}

	return 1
}

func parseVersion(s string) ([]int, string) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")

	pre := ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}

	raw := strings.Split(s, ".")
	parts := make([]int, 0, len(raw))

	for _, r := range raw {
		n, err := strconv.Atoi(r)
		if err != nil {
			break
		}

		parts = append(parts, n)
	}

	return parts, pre
}
