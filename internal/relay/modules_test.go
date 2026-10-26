package relay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
)

func TestSchmorpModules(t *testing.T) {
	data, err := os.ReadFile("../../testdata/schmorp-modules.txt")
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := parseModules(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveModules(definitions, "rxvt-unicode")
	if err != nil {
		t.Fatal(err)
	}
	want := []ModuleMapping{{Source: "libecb", Destination: "libecb"}, {Source: "libev", Destination: "libev"}, {Source: "rxvt-unicode"}}
	if !sameMappings(got, want) {
		t.Fatalf("mapping: got %+v want %+v", got, want)
	}
	for _, listing := range []string{
		"project -a child\nchild -a project\n",
		"project -a project_ project/lib\nproject_ -d project source\nproject/lib -d project/lib source/nested\n",
		"project -a project_ project/lib\nproject_ -d project source\nproject/lib -d ../escape library\n",
		"project -i dangerous source\n",
		"project source selected-file\n",
		"project -a project_ project/lib\nproject_ -d project source\nproject/lib -d elsewhere library\n",
	} {
		defs, err := parseModules([]byte(listing))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolveModules(defs, "project"); err == nil {
			t.Fatalf("unsupported module layout accepted: %s", listing)
		}
	}

}

func TestCompositeModuleImport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureCVS(t, root, "", "init")
	mainBytes := []byte("main project's changes\n")
	dependencyBytes := []byte("dependency\x00\r\n$Id$\xff\n")
	for _, source := range []struct {
		name    string
		content []byte
	}{{"main-source", mainBytes}, {"library", dependencyBytes}} {
		work := t.TempDir()
		writeFixture(t, filepath.Join(work, "Changes"), source.content)
		// Import the actual physical directory independently of the composite
		// alias so the fixture never relies on the importer's mapping model.
		fixtureCVS(t, root, work, "import", "-kb", "-m", "initial", source.name, "VENDOR", "FIRST")
	}
	adminParent := t.TempDir()
	fixtureCVS(t, root, adminParent, "checkout", "CVSROOT")
	admin := filepath.Join(adminParent, "CVSROOT")
	modules := "project -a project_ project/libev\nproject_ -d project main-source\nproject/libev -d project/libev library\n"
	writeFixture(t, filepath.Join(admin, "modules"), []byte(modules))
	fixtureCVS(t, root, admin, "commit", "-m", "define composite module", "modules")
	ctx := context.Background()
	runner := &process.Runner{Log: process.DiscardLogger(), Timeout: time.Minute, MaxOutput: 16 << 20}
	dest := filepath.Join(t.TempDir(), "mirror.git")
	if err := Init(ctx, runner, dest, ":fork:"+root, "project", "composite-fixture", "include", "full"); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, runner, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// This fixture exercises the explicit inclusion mode for composite modules.
	if err := r.ConfigureSource(":fork:"+root, "project", "include"); err != nil {
		t.Fatal(err)
	}
	s, err := r.Sync(ctx, Options{Scope: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Imported != 4 || s.Pending != 0 {
		t.Fatalf("unexpected import summary: %+v", s)
	}
	entries, err := testTreeEntries(t, r, r.Manifest.Refs[trunkRef])
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{"Changes": mainBytes, "libev/Changes": dependencyBytes} {
		data, err := r.git(ctx, nil, nil, "cat-file", "blob", entries[path].OID)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("wrong bytes for %q: got %x want %x", path, data, want)
		}
	}
	for _, mode := range []string{"full"} {
		if _, err := r.Verify(ctx, Options{Scope: "all", Mode: mode}); err != nil {
			t.Fatalf("verify %s: %v", mode, err)
		}
	}
	checkpoint := r.StateOID
	if _, err := r.Sync(ctx, Options{Scope: "all"}); err != nil {
		t.Fatal(err)
	}
	if checkpoint != r.StateOID {
		t.Fatal("unchanged module alias changed Git history")
	}
	r.Close()
	r, err = Open(ctx, runner, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.Config.Policy.Mappings) != 2 {
		t.Fatal("module layout was not recovered from Git")
	}
	writeFixture(t, filepath.Join(admin, "modules"), []byte("project -a project_ project/libev\nproject_ -d project main-source\nproject/libev -d project/libev replacement-library\n"))
	fixtureCVS(t, root, admin, "commit", "-m", "change dependency mapping", "modules")
	if _, err := r.Status(ctx, "all"); diag.Code(err) != "config" {
		t.Fatalf("module remapping was not rejected: %v", err)
	}
}
