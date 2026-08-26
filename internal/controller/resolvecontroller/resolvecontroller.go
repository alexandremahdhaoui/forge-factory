// Package resolvecontroller turns dependency entries into concrete versions.
// A legacy string passes through verbatim; a register entry resolves from the
// register at the pinned revision - consumers consume tags - or from the
// checkout as it stands when no revision is pinned. Resolution is strict: a
// missing package or track fails loud and files a request, an advisory
// pierces every pin, and a pin is never silent.
package resolvecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	spec "github.com/alexandremahdhaoui/forge-register-spec/pkg/registertypes"
	"sigs.k8s.io/yaml"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
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
	// ErrExpired means a hard pin outlived its expires date.
	ErrExpired = errors.New("pin expired")
	// ErrDeprecated means a track's deprecation outlived the register's
	// grace window.
	ErrDeprecated = errors.New("deprecated past grace")
)

// Resolver resolves one language's dependency entries to versions.
type Resolver interface {
	Resolve(ctx context.Context, f config.Factory, root, language string, deps map[string]config.DependencySpec) (map[string]string, []string, error)
	// ResolveTool answers the version pinning one toolchain binary from a
	// register track named "<ecosystem>:<package>".
	ResolveTool(ctx context.Context, f config.Factory, root, track string) (string, []string, error)
}

type Controller struct {
	fs  fsadapter.FS
	git gitadapter.Git
	now func() time.Time

	// warnedUnpushed dedupes the local-only-entries warning per register
	// checkout. Resolution runs sequentially within one sync, so a plain
	// map is enough.
	warnedUnpushed map[string]bool
}

var _ Resolver = (*Controller)(nil)

func New(fs fsadapter.FS, git gitadapter.Git, now func() time.Time) *Controller {
	return &Controller{fs: fs, git: git, now: now, warnedUnpushed: map[string]bool{}}
}

// registerParams is the slice of the register's own config a consumer reads.
// The knobs are register-level; a consumer reads them and never sets them.
type registerParams struct {
	Params struct {
		DeprecatedGraceDays int `json:"deprecatedGraceDays"`
	} `json:"params"`
}

// view reads the register index either from the worktree or, when a revision
// is pinned, from that revision - which is how "consumers consume tags" is
// enforced rather than conventional.
type view struct {
	c   *Controller
	ctx context.Context
	dir string
	rev string
	// url names the register remote, so a failure in a cache
	// materialisation can point at the real register to fix.
	url string
}

func (v view) read(rel string) (string, bool, error) {
	if v.rev != "" {
		return v.c.git.Show(v.ctx, v.dir, v.rev, rel)
	}

	full := filepath.Join(v.dir, filepath.FromSlash(rel))

	exists, err := v.c.fs.Exists(full)
	if err != nil || !exists {
		return "", false, err
	}

	raw, err := v.c.fs.ReadFile(full)
	if err != nil {
		return "", false, err
	}

	return string(raw), true, nil
}

func (v view) list(rel string) ([]string, error) {
	if v.rev != "" {
		return v.c.git.LsTree(v.ctx, v.dir, v.rev, rel)
	}

	return v.c.fs.List(filepath.Join(v.dir, filepath.FromSlash(rel)))
}

// Resolve maps entries to versions. The returned notes are diagnostics the
// driver prints: pins standing, pins to remove, deprecated tracks, requests
// filed.
func (c *Controller) Resolve(ctx context.Context, f config.Factory, root, language string, deps map[string]config.DependencySpec) (map[string]string, []string, error) {
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

		version, entryNotes, err := c.resolveEntry(ctx, f, root, language, name, d)
		notes = append(notes, entryNotes...)

		if err != nil {
			return nil, notes, err
		}

		out[name] = version
	}

	return out, notes, nil
}

// ResolveTool answers the version pinning one toolchain binary: the named
// track's current version, under the same advisory and deprecation rules
// every dependency resolves under. A missing package files an admission
// request, exactly like a dependency would.
func (c *Controller) ResolveTool(ctx context.Context, f config.Factory, root, track string) (string, []string, error) {
	ecosystem, pkg, ok := strings.Cut(track, ":")
	if !ok || ecosystem == "" || pkg == "" {
		return "", nil, fmt.Errorf("toolchain track %q is named <ecosystem>:<package>", track)
	}

	return c.resolveEntry(ctx, f, root, ecosystem, pkg, config.DependencySpec{})
}

func (c *Controller) resolveEntry(ctx context.Context, f config.Factory, root, language, name string, d config.DependencySpec) (string, []string, error) {
	dir, err := c.registerDir(f, root)
	if err != nil {
		return "", nil, err
	}

	v := view{c: c, ctx: ctx, dir: dir, rev: f.Register.Revision, url: f.Register.URL}

	var notes []string

	if note := c.unpushedNote(ctx, v); note != "" {
		notes = append(notes, note)
	}

	track, err := c.track(v, language, name, d)
	if err != nil {
		return "", notes, err
	}

	version := track.Current

	if d.Pin != "" {
		var pinNotes []string

		version, pinNotes, err = c.applyPin(v, language, name, d, track)
		notes = append(notes, pinNotes...)

		if err != nil {
			return "", notes, err
		}
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
		grace := c.graceWindow(dir)
		age := c.now().Sub(track.Deprecated.Since)

		if age > grace {
			return "", notes, fmt.Errorf(
				"%s:%s track %s: %w: deprecated (%s) since %s, past the register's %s grace - move to a successor track",
				language, name, track.Prefix, ErrDeprecated, track.Deprecated.Reason,
				track.Deprecated.Since.Format("2006-01-02"), grace)
		}

		notes = append(notes, fmt.Sprintf(
			"%s:%s track %s is deprecated (%s) since %s - move to a successor track before %s",
			language, name, track.Prefix, track.Deprecated.Reason,
			track.Deprecated.Since.Format("2006-01-02"),
			track.Deprecated.Since.Add(grace).Format("2006-01-02")))
	}

	if d.Wraps != "" {
		version = fmt.Sprintf(d.Wraps, version)
	}

	return version, notes, nil
}

// applyPin floors the track with a soft pin or freezes it with a hard one,
// and always explains itself. A soft pin ahead of its track doubles as the
// upgrade request.
func (c *Controller) applyPin(v view, language, name string, d config.DependencySpec, track spec.Track) (string, []string, error) {
	where := fmt.Sprintf("%s:%s", language, name)

	if d.Mode == "hard" {
		if d.Expires != "" {
			expires, err := parseDate(d.Expires)
			if err != nil {
				return "", nil, fmt.Errorf("%s: reading expires %q: %w", where, d.Expires, err)
			}

			if c.now().After(expires) {
				return "", nil, fmt.Errorf(
					"%s: hard pin %s %w on %s (reason was: %s) - re-decide it or remove it",
					where, d.Pin, ErrExpired, d.Expires, d.Reason)
			}
		}

		return d.Pin, []string{fmt.Sprintf(
			"hard pin %s %s (reason: %s) - the register's track %s is at %s; this consumer is frozen, visibly",
			where, d.Pin, d.Reason, track.Prefix, track.Current)}, nil
	}

	if compareVersions(d.Pin, track.Current) > 0 {
		// A cache materialisation takes no filings: the pin still floors the
		// track, and the retire request waits for a real checkout's sync.
		if c.cacheCheckout(v.dir) {
			return d.Pin, []string{fmt.Sprintf(
				"soft pin %s %s (reason: %s) is ahead of track %s (%s)",
				where, d.Pin, d.Reason, track.Prefix, track.Current)}, nil
		}

		key, err := c.fileRequest(v.dir, language, name, request{
			kind: spec.Upgrade, version: d.Pin, track: track.Prefix, reason: d.Reason,
		})
		if err != nil {
			return "", nil, err
		}

		return d.Pin, []string{fmt.Sprintf(
			"soft pin %s %s (reason: %s) is ahead of track %s (%s) - filed %s so the pin can retire",
			where, d.Pin, d.Reason, track.Prefix, track.Current, key)}, nil
	}

	return track.Current, []string{fmt.Sprintf(
		"soft pin %s %s is behind track %s (%s) - the register is newer; remove this pin",
		where, d.Pin, track.Prefix, track.Current)}, nil
}

// DeadPin reads a note back: when it names a pin for removal, it answers the
// language:name the pin sits on. The note format and this parser live side by
// side, so sync --prune-pins acts on exactly what the resolver said.
func DeadPin(note string) (string, bool) {
	if !strings.HasSuffix(note, "remove this pin") {
		return "", false
	}

	fields := strings.Fields(note)
	if len(fields) < 3 || fields[0] != "soft" || fields[1] != "pin" {
		return "", false
	}

	return fields[2], true
}

// registerDir finds the register checkout: an explicit path, or the directory
// the URL names, rooted in the workspace.
func (c *Controller) registerDir(f config.Factory, root string) (string, error) {
	if f.Register == nil {
		return "", fmt.Errorf("%w: no register block", ErrNoRegister)
	}

	name := f.Register.Path
	if name == "" {
		name = strings.TrimSuffix(path.Base(f.Register.URL), ".git")
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

func (c *Controller) track(v view, language, name string, d config.DependencySpec) (spec.Track, error) {
	prefix := d.Track
	if prefix == "" {
		var err error

		prefix, err = c.defaultTrack(v, language, name)
		if err != nil {
			return spec.Track{}, err
		}
	}

	rel := path.Join("index", language, name, prefix+".json")

	raw, found, err := v.read(rel)
	if err != nil {
		return spec.Track{}, fmt.Errorf("reading track %s: %w", rel, err)
	}

	if !found {
		if c.cacheCheckout(v.dir) {
			return spec.Track{}, unfiledError(v, language, name, ErrUnregistered)
		}

		// The package may carry other tracks: then this is an open-track
		// request, judged by the deny policies, not an admission.
		kind := spec.Admission

		if others, lerr := v.list(path.Join("index", language, name)); lerr == nil && len(others) > 0 {
			kind = spec.OpenTrack
		}

		key, ferr := c.fileRequest(v.dir, language, name, request{
			kind: kind, track: d.Track, version: d.Pin, reason: d.Reason,
		})
		if ferr != nil {
			return spec.Track{}, ferr
		}

		return spec.Track{}, fmt.Errorf(
			"%s:%s track %q is %w at %s; filed %s - the register pipeline answers it, then sync again",
			language, name, prefix, ErrUnregistered, revLabel(v.rev), key)
	}

	var track spec.Track
	if err := json.Unmarshal([]byte(raw), &track); err != nil {
		return spec.Track{}, fmt.Errorf("decoding track %s: %w", rel, err)
	}

	return track, nil
}

// defaultTrack is the highest prefix the register carries for the package.
func (c *Controller) defaultTrack(v view, language, name string) (string, error) {
	entries, err := v.list(path.Join("index", language, name))
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
		if c.cacheCheckout(v.dir) {
			return "", unfiledError(v, language, name, ErrUnregistered)
		}

		key, ferr := c.fileRequest(v.dir, language, name, request{kind: spec.Admission})
		if ferr != nil {
			return "", ferr
		}

		return "", fmt.Errorf(
			"%s:%s is %w at %s; filed %s - the register pipeline answers it, then sync again",
			language, name, ErrUnregistered, revLabel(v.rev), key)
	}

	return best, nil
}

func revLabel(rev string) string {
	if rev == "" {
		return "the checkout"
	}

	return rev
}

type request struct {
	kind    spec.RequestType
	track   string
	version string
	reason  string
}

// cacheCheckout reports whether the register checkout is a materialisation
// under the run cache rather than a checkout a person works in: a cache
// materialisation is a git worktree, whose .git is a link file, while a real
// checkout carries a .git directory. A request filed into the cache is
// answered by nothing, so filing there is worse than refusing.
func (c *Controller) cacheCheckout(dir string) bool {
	gitPath := filepath.Join(dir, ".git")

	exists, err := c.fs.Exists(gitPath)
	if err != nil || !exists {
		return false
	}

	isDir, err := c.fs.IsDir(gitPath)

	return err == nil && !isDir
}

// unpushedNote warns once per register checkout when its entries exist only
// locally: a colleague's fresh clone resolves against origin, so an
// admission that never left this machine reads as "not in the register"
// everywhere else. A cache materialisation is detached and skipped, and a
// checkout that cannot answer (offline, no origin) stays silent.
func (c *Controller) unpushedNote(ctx context.Context, v view) string {
	if v.rev != "" || c.warnedUnpushed[v.dir] || c.cacheCheckout(v.dir) {
		return ""
	}

	c.warnedUnpushed[v.dir] = true

	ahead, _, err := c.git.AheadBehind(ctx, v.dir, "origin/main")
	if err != nil || ahead == 0 {
		return ""
	}

	return fmt.Sprintf(
		"the register checkout %s is %d commit(s) ahead of origin/main - its entries are local-only until pushed, and a fresh clone will not see them",
		v.dir, ahead)
}

// unfiledError is the honest failure for a cache materialisation: nothing is
// filed, because nothing would ever answer it, and the message names the
// real register and the commands that admit the package there.
func unfiledError(v view, language, name string, cause error) error {
	where := v.url
	if where == "" {
		where = v.dir
	}

	return fmt.Errorf(
		"%s:%s is %w at %s. This register copy is a cache materialisation, so filing a request here would reach nothing: admit the package from a real checkout of %s (forge-register add %s:%s --reason \"...\", run its pipeline, push), then rerun",
		language, name, cause, revLabel(v.rev), where, language, name)
}

// fileRequest writes a request into the register checkout's worktree. Filing
// is not writing the index: requests are the register's only door, and the
// pipeline answers them.
func (c *Controller) fileRequest(dir, language, name string, r request) (string, error) {
	now := c.now()

	wire := spec.Request{
		Type:      r.kind,
		Package:   name,
		Ecosystem: spec.RequestEcosystem(language),
		Reason:    "filed by forge-factory sync: the workspace factory names this",
		CreatedAt: now,
	}

	if r.reason != "" {
		wire.Reason = r.reason
	}

	if r.track != "" {
		track := r.track
		wire.Track = &track
	}

	if r.version != "" {
		version := r.version
		wire.Version = &version
	}

	key := language + "/" + name + "/" + strconv.FormatInt(now.Unix(), 10) + "-" + string(r.kind)

	payload, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("encoding the request: %w", err)
	}

	full := filepath.Join(dir, "request", filepath.FromSlash(key)+".json")
	if err := c.fs.WriteFile(full, payload); err != nil {
		return "", fmt.Errorf("filing the request: %w", err)
	}

	return key, nil
}

// graceWindow reads the register's own deprecatedGraceDays. The knob is
// register-level: readable by every consumer, settable by none.
func (c *Controller) graceWindow(dir string) time.Duration {
	const fallback = 30

	raw, err := c.fs.ReadFile(filepath.Join(dir, "forge-register.yaml"))
	if err != nil {
		return fallback * 24 * time.Hour
	}

	var params registerParams
	if err := yaml.Unmarshal(raw, &params); err != nil || params.Params.DeprecatedGraceDays <= 0 {
		return fallback * 24 * time.Hour
	}

	return time.Duration(params.Params.DeprecatedGraceDays) * 24 * time.Hour
}

func parseDate(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("not a date (want 2006-01-02 or RFC3339)")
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

	for i, r := range raw {
		n, err := strconv.Atoi(r)
		if err == nil {
			parts = append(parts, n)

			continue
		}

		// PEP 440 spells pre-releases with no hyphen (1.0.dev5, 1.0rc1):
		// any non-numeric segment joins the pre-release tail, with leading
		// digits kept numeric - the same reading the register uses.
		digits := len(r) - len(strings.TrimLeft(r, "0123456789"))
		if digits > 0 {
			n, _ := strconv.Atoi(r[:digits])
			parts = append(parts, n)
		}

		tail := strings.Join(append([]string{r[digits:]}, raw[i+1:]...), ".")
		if pre == "" {
			pre = tail
		} else {
			pre = tail + "." + pre
		}

		break
	}

	return parts, pre
}
