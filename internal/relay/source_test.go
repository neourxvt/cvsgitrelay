package relay

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
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

// A log message can embed a complete imitation of another rlog file block.
// The parser cannot tell it apart, so the archive name listing must reject it.
func TestRlogFramingImitationRejected(t *testing.T) {
	block := func(name, author, state, log string) string {
		return "RCS file: /cvs/module/" + name + ",v\nhead: 1.1\nbranch:\nlocks: strict\naccess list:\nsymbolic names:\nkeyword substitution: kv\n" +
			"total revisions: 1;\tselected revisions: 1\ndescription:\n----------------------------\nrevision 1.1\n" +
			"date: 2020/01/01 00:00:00;  author: " + author + ";  state: " + state + ";\n" + log
	}
	separator := strings.Repeat("=", 77) + "\n"
	output := block("real", "committer", "Exp", "message\n"+separator+block("forged", "someone-else", "dead", "forged history\n")) + separator
	in, archives, err := parseMappedRlog([]byte(output), []rlogMapping{{RCSPrefix: "/cvs/module/"}}, false)
	if err != nil || len(in.Files) != 2 {
		t.Fatalf("expected the parser alone to accept the imitation: %v", err)
	}
	// Stand in for the CVS client: the name-only listing has no log text.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "cvs"), []byte("#!/bin/sh\nprintf '%s\\n' /cvs/module/real,v\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := &Repository{Config: Config{CVSRoot: "/cvs", Module: "module"}, Runner: testRunner()}
	if err := r.verifyRlogArchives(context.Background(), "module", archives); diag.Code(err) != "source" {
		t.Fatalf("imitated file block was accepted: %v", err)
	}
	if err := r.verifyRlogArchives(context.Background(), "module", archives[:1]); err != nil {
		t.Fatalf("matching archive listing was rejected: %v", err)
	}
}

// Local CVSROOTs ignore the configured remote connection limit and use every
// core; network transports keep the configured limit.
func TestSourceConnections(t *testing.T) {
	for _, root := range []string{"/srv/cvs", ":local:/srv/cvs", ":fork:/srv/cvs"} {
		if got := sourceConnections(root, 1); got != runtime.NumCPU() {
			t.Fatalf("%s: %d connections", root, got)
		}
	}
	for _, root := range []string{":pserver:anonymous@cvs.example.org:/cvs", ":ext:user@host:/cvs"} {
		if got := sourceConnections(root, 1); got != 1 {
			t.Fatalf("%s: %d connections", root, got)
		}
	}
}

// A locked local cvsgitrelay mirror is read once, blocks a concurrent mirror
// update while it is read, and is refused while mirror holds its lock. Every
// other source keeps the confirming second scan.
func TestAcceptSourceScansOfLocalMirrors(t *testing.T) {
	plain := t.TempDir()
	mirror := t.TempDir()
	lockPath := filepath.Join(mirror, mirrorStateDir, "lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// tryMirrorLock reports whether mirror could take its exclusive lock now.
	tryMirrorLock := func() bool {
		f, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
	}
	accept := func(root string) (int, error) {
		calls := 0
		r := &Repository{Config: Config{CVSRoot: root, SourceID: "source"}, Runner: testRunner()}
		r.sourceReader = func(context.Context) (canonical.Inventory, sourceStats, error) {
			calls++
			if root == mirror && tryMirrorLock() {
				t.Fatal("mirror could update the source during the scan")
			}
			return canonical.Inventory{}, sourceStats{}, nil
		}
		_, _, err := r.acceptSource(context.Background(), &Options{})
		return calls, err
	}
	for root, want := range map[string]int{plain: 2, ":local:" + plain: 2, mirror: 1, ":fork:" + mirror: 1, ":pserver:anonymous@cvs.example.org:" + mirror: 2} {
		calls, err := accept(root)
		if err != nil || calls != want {
			t.Fatalf("%s: %d scans, %v", root, calls, err)
		}
	}
	if !tryMirrorLock() {
		t.Fatal("the scan did not release the mirror lock")
	}
	held, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if calls, err := accept(mirror); diag.Code(err) != "locked" || calls != 0 {
		t.Fatalf("scanned a mirror during its update: %d scans, %v", calls, err)
	}
}

// Both maps are part of the destination's policy and cannot change later.
func TestRequestedPolicyMaps(t *testing.T) {
	r := &Repository{Config: Config{SourceID: "s", Policy: canonical.Policy{Verify: "full", Format: 3, Authors: map[string]string{"a": "A <a@x>"}, Branches: map[string]string{"trunk": "main"}}}}
	if err := r.ValidateRequestedPolicy("s", "full", map[string]string{"a": "A <a@x>"}, map[string]string{"trunk": "main"}); err != nil {
		t.Fatal(err)
	}
	for _, branches := range []map[string]string{nil, {}, {"trunk": "master"}, {"trunk": "main", "x": "y"}} {
		if err := r.ValidateRequestedPolicy("s", "full", map[string]string{"a": "A <a@x>"}, branches); err == nil {
			t.Fatalf("branch map %v accepted", branches)
		}
	}
	// A destination without a map treats nil and an empty table alike.
	r.Config.Policy.Branches = nil
	if err := r.ValidateRequestedPolicy("s", "full", map[string]string{"a": "A <a@x>"}, map[string]string{}); err != nil {
		t.Fatal(err)
	}
}
