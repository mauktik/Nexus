package apt

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// foreignHost is a host whose native architecture is amd64, with i386 added
// (dpkg --add-architecture): nxlib is installed for amd64 and i386, nxold
// for i386 only, nxg++ for amd64 and nxall as all. apt knows the same
// names in its indexes.
func foreignHost(t *testing.T) (*Provider, *[][]string) {
	t.Helper()
	calls := &[][]string{}
	p := New(Options{Host: "testhost"})
	p.root = t.TempDir()
	dpkgRows := "nxlib\tamd64\t1.0\tinstalled\nnxlib\ti386\t1.0\tinstalled\nnxold\ti386\t1.0\tinstalled\n" +
		"nxg++\tamd64\t1.0\tinstalled\nnxall\tall\t1.0\tinstalled\n"
	records := map[string]string{
		"nxlib": "Package: nxlib\nArchitecture: amd64\nVersion: 1.0\n\nPackage: nxlib\nArchitecture: i386\nVersion: 1.0\n\n",
		"nxold": "Package: nxold\nArchitecture: i386\nVersion: 1.0\n\n",
		"nxg++": "Package: nxg++\nArchitecture: amd64\nVersion: 1.0\n\n",
	}
	p.run = func(_ context.Context, c command) (result, error) {
		*calls = append(*calls, slices.Clone(c.Argv))
		i := slices.Index(c.Argv, "--")
		switch {
		case c.Argv[0] == Dpkg:
			return result{Stdout: []byte("amd64\n")}, nil
		case c.Argv[0] == AptCache && slices.Contains(c.Argv, "pkgnames"):
			return result{Stdout: []byte("nxall\nnxg++\nnxlib\nnxold\n")}, nil
		case c.Argv[0] == AptCache && slices.Contains(c.Argv, "show") && i > 0:
			var out strings.Builder
			for _, n := range c.Argv[i+1:] {
				out.WriteString(records[strings.SplitN(n, ":", 2)[0]])
			}
			return result{Stdout: []byte(out.String())}, nil
		case c.Argv[0] == DpkgQuery && i > 0:
			var out strings.Builder
			for _, n := range c.Argv[i+1:] {
				for _, row := range strings.SplitAfter(dpkgRows, "\n") {
					if strings.HasPrefix(row, strings.SplitN(n, ":", 2)[0]+"\t") {
						out.WriteString(row)
					}
				}
			}
			return result{Stdout: []byte(out.String())}, nil
		case c.Argv[0] == DpkgQuery:
			return result{Stdout: []byte(dpkgRows)}, nil
		}
		t.Fatalf("unexpected command %q", c.Argv)
		return result{}, nil
	}
	return p, calls
}

// CR-27: a name of a foreign architecture is passed to apt-get as name:arch,
// and a name of the native architecture, or of all, as the bare name. The
// branch that builds name:arch is the one apt-get needs to tell nxlib:i386
// from nxlib, and it was never run.
func TestForeignArchitectureResolution(t *testing.T) {
	ctx := context.Background()
	p, _ := foreignHost(t)

	// Install: apt's indexes. The native name stays bare, the foreign one
	// is qualified, and an architecture apt has no package for is not-found.
	got, err := p.resolveAvailable(ctx, []string{"nxlib", "nxlib:i386", "nxlib:amd64", "nxold:i386"})
	if want := []string{"nxlib", "nxlib:i386", "nxold:i386"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("install: %q, %v, want %q", got, err, want)
	}
	for _, n := range []string{"nxold", "nxlib:arm64"} {
		if _, err := p.resolveAvailable(ctx, []string{n}); slugOf(t, err) != problem.NotFound {
			t.Errorf("install %s: %v, want not-found", n, err)
		}
	}

	// Upgrade and remove: dpkg's database.
	got, err = p.resolveInstalled(ctx, []string{"nxlib", "nxlib:i386", "nxold:i386", "nxall"})
	if want := []string{"nxlib", "nxlib:i386", "nxold:i386", "nxall"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("upgrade: %q, %v, want %q", got, err, want)
	}
	got, err = p.resolveRemovable(ctx, []string{"nxold:i386", "nxg++"}, false)
	if want := []string{"nxold:i386", "nxg++"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("remove: %q, %v, want %q", got, err, want)
	}
	if _, err := p.resolveRemovable(ctx, []string{"nxold"}, false); slugOf(t, err) != problem.NotFound {
		t.Errorf("remove nxold (installed for i386 only): %v, want not-found", err)
	}

	// The installed list names the same packages the same way, which is the
	// name apt-get -s prints in a preview.
	list, err := p.installed(ctx)
	var names []string
	for _, i := range list {
		names = append(names, i.Name)
	}
	if want := []string{"nxall", "nxg++", "nxlib", "nxlib:i386", "nxold:i386"}; err != nil || !slices.Equal(names, want) {
		t.Errorf("installed list: %q, %v, want %q", names, err, want)
	}
}

// The commands built from resolved names keep a "+" name whole and a
// foreign name qualified, each after "--".
func TestForeignAndPlusNamesReachAptGetAfterTheSeparator(t *testing.T) {
	for name, tc := range map[string]struct{ got, want []string }{
		"remove nxg++":        {removeCommand([]string{"nxg++"}, false, false), []string{"remove", "--", "nxg++"}},
		"purge foreign":       {removeCommand([]string{"nxold:i386", "nxg++"}, true, false), []string{"purge", "--", "nxold:i386", "nxg++"}},
		"install foreign pin": {installCommand([]string{"nxlib:i386"}, "1.0"), []string{"install", "--no-remove", "--", "nxlib:i386=1.0"}},
		"upgrade subset":      {upgradeArgs(spec.ModeStandard, []string{"nxg++", "nxlib:i386"}), []string{"install", "--only-upgrade", "--no-remove", "--", "nxg++", "nxlib:i386"}},
		"full upgrade subset": {upgradeArgs(spec.ModeFull, []string{"nxg++"}), []string{"install", "--only-upgrade", "--", "nxg++"}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s: %q, want %q", name, tc.got, tc.want)
		}
	}
}
