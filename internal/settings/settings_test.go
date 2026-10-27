package settings

import (
	"os"
	"path/filepath"
	"testing"
)

const base = "[source]\nsource_id = \"s\"\ncvsroot = \"/cvs\"\nmodule = \"m\"\n[destination]\nrepo = \"r.git\"\n"

// Usernames with dots must be quoted TOML keys; identities are checked early.
func TestAuthorMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	write := func(authors string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(base+"[authors]\n"+authors), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("root = \"Root User <root@example.com>\"\n\"j.doe\" = \"Jane Doe <jane@example.com>\"\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Authors["root"] != "Root User <root@example.com>" || c.Authors["j.doe"] != "Jane Doe <jane@example.com>" {
		t.Fatalf("unexpected author map: %v", c.Authors)
	}
	for _, bad := range []string{"root = \"Root User\"\n", "root = \"<root@example.com>\"\n", "j.doe = \"Jane Doe <jane@example.com>\"\n"} {
		write(bad)
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid author map accepted: %q", bad)
		}
	}
}

// The documented example configuration must stay loadable.
func TestExampleLoads(t *testing.T) {
	if _, err := Load("../../example.toml"); err != nil {
		t.Fatal(err)
	}
}

// The branch map renames CVS branches; names with dots or slashes need quotes.
func TestBranchMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	write := func(branches string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(base+"[branches]\n"+branches), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("trunk = \"main\"\n\"REL-9_30\" = \"release/9.30\"\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Branches["trunk"] != "main" || c.Branches["REL-9_30"] != "release/9.30" || len(c.Branches) != 2 {
		t.Fatalf("unexpected branch map: %v", c.Branches)
	}
	for _, bad := range []string{"trunk = \"bad name\"\n", "trunk = \"\"\n", "trunk = \"main\"\nother = \"main\"\n", "trunk = \"a\"\nother = \"a/b\"\n", "trunk = \"main..x\"\n"} {
		write(bad)
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid branch map accepted: %q", bad)
		}
	}
}
