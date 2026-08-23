package runcontroller

import "testing"

func TestSplitTargetShapes(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, module, sub, rev string }{
		{"github.com/x/repo", "github.com/x/repo", "", ""},
		{"github.com/x/repo/cmd/tool", "github.com/x/repo", "cmd/tool", ""},
		{"github.com/x/repo/cmd/tool@v1.2.0", "github.com/x/repo", "cmd/tool", "v1.2.0"},
		{"github.com/x/repo@main", "github.com/x/repo", "", "main"},
	}
	for _, c := range cases {
		module, sub, rev := splitTarget(c.in)
		if module != c.module || sub != c.sub || rev != c.rev {
			t.Errorf("splitTarget(%q) = %q %q %q", c.in, module, sub, rev)
		}
	}
}

func TestSplitRevKeepsSSHUserinfo(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, url, rev string }{
		{"git@github.com:x/f.git", "git@github.com:x/f.git", ""},
		{"git@github.com:x/f.git@v0.2.0", "git@github.com:x/f.git", "v0.2.0"},
		{"/abs/path", "/abs/path", ""},
		{"/abs/path@abc123", "/abs/path", "abc123"},
	}
	for _, c := range cases {
		url, rev := splitRev(c.in)
		if url != c.url || rev != c.rev {
			t.Errorf("splitRev(%q) = %q %q", c.in, url, rev)
		}
	}
}

func TestModuleToURLForms(t *testing.T) {
	t.Parallel()

	if got := moduleToURL("github.com/x/repo"); got != "git@github.com:x/repo.git" {
		t.Errorf("moduleToURL = %q", got)
	}
	if got := moduleToURL("/abs/path"); got != "/abs/path" {
		t.Errorf("an absolute path clones as itself, got %q", got)
	}
	if got := moduleToURL("lonesegment"); got != "lonesegment" {
		t.Errorf("moduleToURL = %q", got)
	}
}

func TestIsModulePathForms(t *testing.T) {
	t.Parallel()

	for target, want := range map[string]bool{
		"./cmd/tool":        false,
		"../elsewhere":      false,
		"my-tool":           false,
		"github.com/x/repo": true,
		"/abs/repo":         true,
	} {
		if got := isModulePath(target); got != want {
			t.Errorf("isModulePath(%q) = %v", target, got)
		}
	}
}

func TestShortShaAndSanitize(t *testing.T) {
	t.Parallel()

	if got := shortSha("abcdefabcdefabcdef"); got != "abcdefabcdef" {
		t.Errorf("shortSha = %q", got)
	}
	if got := shortSha("abc"); got != "abc" {
		t.Errorf("shortSha = %q", got)
	}
	if got := sanitize("git@github.com:x/y.git"); got == "" || got == "git@github.com:x/y.git" {
		t.Errorf("sanitize = %q", got)
	}
}
