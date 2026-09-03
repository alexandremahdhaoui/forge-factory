package describecontroller_test

import (
	"regexp"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/describecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type describer interface {
	Describe(runtimetypes.Input) (runtimetypes.Description, error)
}

// Every language describer answers a complete, deterministic description
// for both linux platforms: https url, a real sha256, an unpack kind the
// install engine lays out, and at least one exposed executable.
func TestEveryDescriberAnswersBothLinuxPlatforms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		d    describer
		in   runtimetypes.Input
	}{
		{"go", describecontroller.Go{}, runtimetypes.Input{Runtime: "go", Version: "1.26.5"}},
		{"rust", describecontroller.Rust{}, runtimetypes.Input{Runtime: "rust", Version: "1.97.0"}},
		{"python", describecontroller.Python{}, runtimetypes.Input{Runtime: "python", Version: "3.13.7"}},
		{"typescript", describecontroller.TypeScript{}, runtimetypes.Input{Runtime: "typescript", Version: "22.22.2"}},
		{"jre", describecontroller.Archive{}, runtimetypes.Input{Runtime: "jre", Version: "21.0.5+11"}},
		{"pnpm", describecontroller.Archive{}, runtimetypes.Input{Runtime: "pnpm", Version: "10.33.0"}},
		{"uv", describecontroller.Archive{}, runtimetypes.Input{Runtime: "uv", Version: "0.8.17"}},
		{"bun", describecontroller.Archive{}, runtimetypes.Input{Runtime: "bun", Version: "1.3.14"}},
		{"openapi-generator", describecontroller.Archive{}, runtimetypes.Input{
			Runtime: "openapi-generator", Version: "7.19.0",
		}},
		{"cargo-deny", describecontroller.Archive{}, runtimetypes.Input{Runtime: "cargo-deny", Version: "0.20.2"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, arch := range []string{"amd64", "arm64"} {
				in := tc.in
				in.OS, in.Arch = "linux", arch

				out, err := tc.d.Describe(in)
				require.NoError(t, err, arch)

				assert.Equal(t, in.Runtime, out.Runtime)
				assert.Equal(t, in.Version, out.Version)
				require.NotEmpty(t, out.Artifacts, arch)

				for _, a := range out.Artifacts {
					assert.Regexp(t, `^https://`, a.URL)
					assert.Regexp(t, sha256Hex, a.SHA256)
					assert.Contains(t, []string{"tar", "tar-gz", "zip", "file"}, a.Unpack)
				}
			}
		})
	}
}

func TestTheTwoArchitecturesPinDifferentBytes(t *testing.T) {
	t.Parallel()

	amd, err := describecontroller.Go{}.Describe(
		runtimetypes.Input{Runtime: "go", Version: "1.26.5", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)

	arm, err := describecontroller.Go{}.Describe(
		runtimetypes.Input{Runtime: "go", Version: "1.26.5", OS: "linux", Arch: "arm64"})
	require.NoError(t, err)

	assert.NotEqual(t, amd.Artifacts[0].SHA256, arm.Artifacts[0].SHA256)
	assert.NotEqual(t, amd.Artifacts[0].URL, arm.Artifacts[0].URL)
}

// An unknown version refuses naming the fix - the table is a reviewed pin,
// not a lookup that retries.
func TestAnUnknownVersionRefusesNamingTheFix(t *testing.T) {
	t.Parallel()

	for name, d := range map[string]describer{
		"go": describecontroller.Go{}, "rust": describecontroller.Rust{},
		"python": describecontroller.Python{}, "typescript": describecontroller.TypeScript{},
		"archive": describecontroller.Archive{},
	} {
		_, err := d.Describe(runtimetypes.Input{Runtime: name, Version: "0.0.0", OS: "linux", Arch: "amd64"})
		require.ErrorIs(t, err, describecontroller.ErrVersion, name)
		require.ErrorContains(t, err, "add the version's published sha256", name)
	}
}

func TestAnUnknownPlatformRefuses(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]struct {
		d describer
		i runtimetypes.Input
	}{
		"go":   {describecontroller.Go{}, runtimetypes.Input{Runtime: "go", Version: "1.26.5"}},
		"rust": {describecontroller.Rust{}, runtimetypes.Input{Runtime: "rust", Version: "1.97.0"}},
		"pnpm": {describecontroller.Archive{}, runtimetypes.Input{Runtime: "pnpm", Version: "10.33.0"}},
	} {
		i := in.i
		i.OS, i.Arch = "plan9", "mips"

		_, err := in.d.Describe(i)
		require.ErrorIs(t, err, describecontroller.ErrPlatform, name)
	}
}

// The language facts live with the language: go and rust assume a C
// toolchain and say so; the jar assumes a JVM.
func TestPrerequisitesAreDeclaredWhereTheFactLives(t *testing.T) {
	t.Parallel()

	goDesc, err := describecontroller.Go{}.Describe(
		runtimetypes.Input{Runtime: "go", Version: "1.26.5", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)
	require.Len(t, goDesc.Prerequisites, 1)
	assert.Equal(t, "c-compiler", goDesc.Prerequisites[0].Name)
	assert.Contains(t, goDesc.Prerequisites[0].Reason, "-race")

	jar, err := describecontroller.Archive{}.Describe(
		runtimetypes.Input{Runtime: "openapi-generator", Version: "7.19.0", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)
	require.Len(t, jar.Prerequisites, 1)
	assert.Equal(t, "jre", jar.Prerequisites[0].Name)
	assert.Equal(t, "java", jar.Prerequisites[0].Verify)
}

// Rust's combined tarball merges components into one prefix; the picks are
// that merge as data, and the std pick carries the platform's triple.
func TestRustPicksCarryTheTriple(t *testing.T) {
	t.Parallel()

	out, err := describecontroller.Rust{}.Describe(
		runtimetypes.Input{Runtime: "rust", Version: "1.97.0", OS: "linux", Arch: "arm64"})
	require.NoError(t, err)

	var froms []string
	for _, p := range out.Artifacts[0].Picks {
		froms = append(froms, p.From)
	}

	assert.ElementsMatch(t, []string{
		"rustc", "cargo", "rust-std-aarch64-unknown-linux-gnu",
		"rustfmt-preview", "clippy-preview",
	}, froms)
}

// The JRE provides the capability the jar assumes, and its env names the
// prefix - the {prefix} token the expose step replaces.
func TestTheJreProvidesWhatTheJarNeeds(t *testing.T) {
	t.Parallel()

	jre, err := describecontroller.Archive{}.Describe(
		runtimetypes.Input{Runtime: "jre", Version: "21.0.5+11", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)
	assert.Equal(t, []string{"jre"}, jre.Provides)
	assert.Equal(t, "{prefix}", jre.Env["JAVA_HOME"])
}

// The jar is architecture-independent: one artifact under the "any" key
// serves every platform.
func TestAnArchIndependentArtifactServesAnyPlatform(t *testing.T) {
	t.Parallel()

	a, err := describecontroller.Archive{}.Describe(
		runtimetypes.Input{Runtime: "openapi-generator", Version: "7.19.0", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)

	b, err := describecontroller.Archive{}.Describe(
		runtimetypes.Input{Runtime: "openapi-generator", Version: "7.19.0", OS: "linux", Arch: "arm64"})
	require.NoError(t, err)

	assert.Equal(t, a.Artifacts[0].SHA256, b.Artifacts[0].SHA256)
}

// A file artifact with an at-only pick is renamed at install: the release
// asset is pnpm-linux-x64, the exposed binary is pnpm.
func TestPnpmRenamesItsReleaseAsset(t *testing.T) {
	t.Parallel()

	out, err := describecontroller.Archive{}.Describe(
		runtimetypes.Input{Runtime: "pnpm", Version: "10.33.0", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)
	require.Len(t, out.Artifacts[0].Picks, 1)
	assert.Equal(t, "pnpm", out.Artifacts[0].Picks[0].At)
	assert.Equal(t, []string{"pnpm"}, out.Bins)
}

func TestCargoDenyExposesItsBinaryOnThePrefixBin(t *testing.T) {
	t.Parallel()

	out, err := describecontroller.Archive{}.Describe(
		runtimetypes.Input{Runtime: "cargo-deny", Version: "0.20.2", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)
	assert.Equal(t, []string{"cargo-deny"}, out.Bins)
}

func TestEachLanguageDescriberNamesItsLanguage(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "go", describecontroller.Go{}.Language())
	assert.Equal(t, "rust", describecontroller.Rust{}.Language())
	assert.Equal(t, "python", describecontroller.Python{}.Language())
	assert.Equal(t, "typescript", describecontroller.TypeScript{}.Language())
}

func TestAKnownArchiveRuntimeWithAnUnknownVersionRefuses(t *testing.T) {
	t.Parallel()

	_, err := describecontroller.Archive{}.Describe(
		runtimetypes.Input{Runtime: "jre", Version: "0.0.0", OS: "linux", Arch: "amd64"})
	require.ErrorIs(t, err, describecontroller.ErrVersion)
}
