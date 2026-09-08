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

func TestRustWithNoTargetsAnswersTheHostToolchainAlone(t *testing.T) {
	t.Parallel()

	out, err := describecontroller.Rust{}.Describe(
		runtimetypes.Input{Runtime: "rust", Version: "1.97.0", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)

	require.Len(t, out.Artifacts, 1)
	assert.Equal(t,
		"https://static.rust-lang.org/dist/2026-07-09/rust-1.97.0-x86_64-unknown-linux-gnu.tar.gz",
		out.Artifacts[0].URL)
	assert.Equal(t, "eb89b20287153391c49ebcdb7fd91b683a12438d129bfb92eadcc495545af3a7", out.Artifacts[0].SHA256)
	assert.Len(t, out.Artifacts[0].Picks, 5)
}

func TestRustWithOneTargetAppendsThatTargetsStdBesideTheHostStd(t *testing.T) {
	t.Parallel()

	out, err := describecontroller.Rust{}.Describe(runtimetypes.Input{
		Runtime: "rust", Version: "1.97.0", OS: "linux", Arch: "amd64",
		Targets: []string{"x86_64-pc-windows-gnu"},
	})
	require.NoError(t, err)

	require.Len(t, out.Artifacts, 2)

	std := out.Artifacts[1]
	assert.Equal(t,
		"https://static.rust-lang.org/dist/2026-07-09/rust-std-1.97.0-x86_64-pc-windows-gnu.tar.gz",
		std.URL)
	assert.Equal(t, "7ef19232b379616509007c7a8db45c4ee7031c22d9614fa2b7f18b2a30ff85fa", std.SHA256)
	assert.Equal(t, "tar-gz", std.Unpack)
	assert.Equal(t, 1, std.Strip)
	assert.Equal(t, []runtimetypes.Pick{{From: "rust-std-x86_64-pc-windows-gnu"}}, std.Picks)

	assert.Len(t, out.Artifacts[0].Picks, 5)
	assert.Len(t, out.Bins, 7)
}

func TestRustWithTwoTargetsAppendsOneStdPerTargetInDeclaredOrder(t *testing.T) {
	t.Parallel()

	out, err := describecontroller.Rust{}.Describe(runtimetypes.Input{
		Runtime: "rust", Version: "1.97.0", OS: "linux", Arch: "amd64",
		Targets: []string{"x86_64-pc-windows-gnu", "aarch64-unknown-linux-gnu"},
	})
	require.NoError(t, err)

	require.Len(t, out.Artifacts, 3)

	assert.Equal(t,
		"https://static.rust-lang.org/dist/2026-07-09/rust-std-1.97.0-x86_64-pc-windows-gnu.tar.gz",
		out.Artifacts[1].URL)
	assert.Equal(t, []runtimetypes.Pick{{From: "rust-std-x86_64-pc-windows-gnu"}}, out.Artifacts[1].Picks)

	assert.Equal(t,
		"https://static.rust-lang.org/dist/2026-07-09/rust-std-1.97.0-aarch64-unknown-linux-gnu.tar.gz",
		out.Artifacts[2].URL)
	assert.Equal(t, "ae6c76b70be4768ecf2f320f1e6fa28525730b7fd7d6cfc822a3a23e0c07fcc2", out.Artifacts[2].SHA256)
	assert.Equal(t, []runtimetypes.Pick{{From: "rust-std-aarch64-unknown-linux-gnu"}}, out.Artifacts[2].Picks)

	assert.NotEqual(t, out.Artifacts[1].SHA256, out.Artifacts[2].SHA256)
}

func TestRustSkipsATargetThatIsTheHostTripleBecauseTheHostTarballCarriesIt(t *testing.T) {
	t.Parallel()

	out, err := describecontroller.Rust{}.Describe(runtimetypes.Input{
		Runtime: "rust", Version: "1.97.0", OS: "linux", Arch: "arm64",
		Targets: []string{"aarch64-unknown-linux-gnu", "x86_64-unknown-linux-gnu"},
	})
	require.NoError(t, err)

	require.Len(t, out.Artifacts, 2)
	assert.Equal(t, []runtimetypes.Pick{{From: "rust-std-x86_64-unknown-linux-gnu"}}, out.Artifacts[1].Picks)
	assert.Equal(t, "26abd06b9c4221811af1baec86dfb6de9535862fc853b85388dcc314c96cea6d", out.Artifacts[1].SHA256)
}

func TestRustRefusesATargetWithNoStdInTheTableNamingTheFix(t *testing.T) {
	t.Parallel()

	_, err := describecontroller.Rust{}.Describe(runtimetypes.Input{
		Runtime: "rust", Version: "1.97.0", OS: "linux", Arch: "amd64",
		Targets: []string{"wasm32-unknown-unknown"},
	})
	require.ErrorIs(t, err, describecontroller.ErrTarget)
	require.ErrorContains(t, err, "wasm32-unknown-unknown")
	require.ErrorContains(t, err, "add the target's published sha256")
}

func TestTheJreProvidesWhatTheJarNeeds(t *testing.T) {
	t.Parallel()

	jre, err := describecontroller.Archive{}.Describe(
		runtimetypes.Input{Runtime: "jre", Version: "21.0.5+11", OS: "linux", Arch: "amd64"})
	require.NoError(t, err)
	assert.Equal(t, []string{"jre"}, jre.Provides)
	assert.Equal(t, "{prefix}", jre.Env["JAVA_HOME"])
}

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
