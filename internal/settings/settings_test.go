package settings

import (
	"os"
	"path/filepath"
	"testing"

	"cvsgitrelay/internal/secret"
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

// The remote is optional and must not embed credentials.
func TestRemote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	write := func(remote string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(base+"remote = \""+remote+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("git@github.com:owner/repo.git")
	if c, err := Load(path); err != nil || c.Destination.Remote != "git@github.com:owner/repo.git" {
		t.Fatalf("remote: %q, %v", c.Destination.Remote, err)
	}
	write("https://user:token@github.com/owner/repo.git")
	if _, err := Load(path); err == nil {
		t.Fatal("a remote with a token was accepted")
	}
}

// The remote secret must decode, and a token needs an https remote.
func TestRemoteSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	encoded, err := secret.Encode("ghp_token")
	if err != nil {
		t.Fatal(err)
	}
	load := func(remote, value string) (Config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(base+"remote = \""+remote+"\"\nremote_secret = \""+value+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	c, err := load("https://github.com/o/r.git", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := c.RemoteToken(); err != nil || token != "ghp_token" {
		t.Fatalf("token %q, %v", token, err)
	}
	for name, tc := range map[string][2]string{
		"plain text":  {"https://github.com/o/r.git", "ghp_token"},
		"ssh remote":  {"git@github.com:o/r.git", encoded},
		"http remote": {"http://github.com/o/r.git", encoded},
	} {
		if _, err := load(tc[0], tc[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A file holds the secret too; a relative path belongs to the TOML's directory.
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "token.secret"), []byte(encoded+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"token.secret", filepath.Join(filepath.Dir(path), "token.secret")} {
		c, err := load("https://github.com/o/r.git", value)
		if err != nil {
			t.Fatalf("%s: %v", value, err)
		}
		if token, err := c.RemoteToken(); err != nil || token != "ghp_token" {
			t.Fatalf("file %s: token %q, %v", value, token, err)
		}
	}
	if _, err := load("https://github.com/o/r.git", "missing.secret"); err == nil {
		t.Error("a missing secret file was accepted")
	}
	// --remote "" turns pushing off and leaves the secret unused.
	c, err = load("", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if token, _ := c.RemoteToken(); token != "" {
		t.Errorf("token %q without a remote", token)
	}
}

// Unattended operation is the default: GC and content checks run monthly,
// changed history is accepted, and an omitted verification mode is left to
// the destination instead of being pinned here.
func TestUnattendedDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verify.Mode != "" || c.Maintenance.IntervalDays != 30 || c.Maintenance.VerifyDays != 30 || !c.Sync.AcceptHistoryChanges {
		t.Fatalf("unexpected defaults: %+v %+v %+v", c.Verify, c.Maintenance, c.Sync)
	}
	for _, mode := range []string{"full", "trust-append-only"} {
		if err := os.WriteFile(path, []byte(base+"[verify]\nmode = \""+mode+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if c, err := Load(path); err != nil || c.Verify.Mode != mode {
			t.Fatalf("explicit mode %s: %q, %v", mode, c.Verify.Mode, err)
		}
	}
}
