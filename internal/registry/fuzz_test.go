package registry

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func FuzzParse(f *testing.F) {
	for _, s := range []string{"mcp:io.github.org/server@1.2.0", "skill:https://github.com/org/skills//pdf@v1", "skill:org/repo@main",
		"plugin:name@org/marketplace@v2", "pi:@scope/pkg@1.0.0", "pi:git:https://github.com/a/b@abc", "skill:repo//../x", "mcp:-x", "nope:x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := Parse(s)
		if err != nil {
			return
		}
		if r.Name == "" || strings.HasPrefix(r.Source, "-") || strings.HasPrefix(r.Version, "-") {
			t.Fatalf("%q → %+v", s, r)
		}
		if r.Kind == "skill" && (r.Name == ".." || strings.HasPrefix(r.Name, "../") || strings.HasPrefix(r.Name, "/")) {
			t.Fatalf("%q → %+v: the subpath escapes", s, r)
		}
	})
}

func FuzzReadLock(f *testing.F) {
	f.Add([]byte("[[extension]]\nresolved = \"https://x/y.tgz\"\nintegrity = \"sha512-abc\"\n[extension.ref]\nkind = \"pi\"\nname = \"p\"\nversion = \"1.0.0\"\n"))
	f.Add([]byte(""))
	f.Add([]byte("[[extension]]\ncommand = [\"a\", \"b\"]\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		l, err := parseLock(b)
		if err != nil {
			return
		}
		out, err := toml.Marshal(l)
		if err != nil {
			return // a value the encoder can't write (not ours to fix)
		}
		if _, err := parseLock(out); err != nil {
			t.Fatalf("a lock Warden wrote does not read back: %v\n%s", err, out)
		}
	})
}
