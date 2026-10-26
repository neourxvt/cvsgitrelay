package relay

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run the real CVS client/server protocol through its remote-shell transport.
// The importer sees only CVS responses; it never reads source archive files.
func TestV2RemoteCVSImport(t *testing.T) {
	root, work, _ := fixture(t)
	dir := t.TempDir()
	rsh := filepath.Join(dir, "remote-shell")
	log := filepath.Join(dir, "requests")
	t.Setenv("CVSGITRELAY_TEST_REMOTE_LOG", log)
	t.Setenv("CVS_RSH", rsh)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CVSGITRELAY_TEST_REMOTE_LOG\"\nexec cvs server\n"
	if err := os.WriteFile(rsh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	remote := openFixture(t, ":ext:reader@fixture:"+root, "full")
	if _, err := remote.Sync(ctx, Options{Connections: 4}); err != nil {
		t.Fatal(err)
	}
	in, _, err := remote.readSourceScope(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range in.Records {
		if record.Present != (record.State == "live") || (record.State == "dead" && len(record.Content) != 0) {
			t.Fatal("remote content-presence contract violated")
		}
	}
	// A newly observed branch member must converge with a fresh import over a
	// different transport, independently of the earlier publication boundary.
	writeFixture(t, filepath.Join(work, "late"), []byte("late remote member\n"))
	fixtureCVS(t, root, work, "add", "late")
	fixtureCVS(t, root, work, "commit", "-m", "late remote member", "late")
	if _, err := remote.Sync(ctx, Options{Connections: 1}); err != nil {
		t.Fatal(err)
	}
	fresh := openFixture(t, ":fork:"+root, "full")
	if _, err := fresh.Sync(ctx, Options{Connections: 4}); err != nil {
		t.Fatal(err)
	}
	if fresh.StateOID != remote.StateOID || !maps.Equal(fresh.Manifest.Refs, remote.Manifest.Refs) || !maps.Equal(fresh.Manifest.Revisions, remote.Manifest.Revisions) {
		t.Fatal("fresh and incremental remote generations differ")
	}
	requests, err := os.ReadFile(log)
	if err != nil || !bytes.Contains(requests, []byte("fixture")) || !bytes.Contains(requests, []byte("server")) {
		t.Fatalf("remote CVS server was not used: %s: %v", requests, err)
	}
	state := remote.StateOID
	summary, err := remote.Sync(ctx, Options{Connections: 4})
	if err != nil || remote.StateOID != state || summary.ObjectWrites != 0 || summary.SourceRequests < 14 {
		t.Fatalf("remote no-op did not verify without publishing: %+v: %v", summary, err)
	}
}

// A complete listing, branch declarations and existing forkpoints establish
// the supported logical ancestry. Inaccessible forkpoints are never invented.
func TestReportedAncestry(t *testing.T) {
	data, err := os.ReadFile("../../testdata/gnu-cvs-1.11.23.rlog")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, old, replacement string
		valid                  bool
	}{
		{"complete", "", "", true},
		{"numbering-gap", "1.2", "1.4", true},
		{"missing-declaration", "branches:  1.2.2;", "branches:", false},
		{"wrong-fork", "branches:  1.2.2;", "branches:  1.1.2;", false},
		{"inaccessible-fork", "revision 1.2\n", "revision 1.3\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := data
			if tc.old != "" {
				input = []byte(strings.ReplaceAll(string(data), tc.old, tc.replacement))
			}
			in, err := parseMappedRlogMode(input, []rlogMapping{{RCSPrefix: "/fixture/cvsroot/module/", SourcePrefix: "module"}}, false)
			if err == nil {
				err = validateReportedAncestry(in)
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if tc.name == "numbering-gap" && in.Revisions[key("active", "1.4")].Predecessor != "1.1" {
				t.Fatal("parent was inferred by decrementing a revision number")
			}
		})
	}
}
