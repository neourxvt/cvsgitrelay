package relay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cvsgitrelay/internal/process"
)

func TestSourceLocatorsAndPaths(t *testing.T) {
	if compareNumber("1.9", "1.10") >= 0 {
		t.Fatal("revision ordering was lexicographic")
	}
	if b, ok := branchNumber("1.3.0.2"); !ok || b != "1.3.2" {
		t.Fatal("magic branch number was not normalized")
	}
	for _, root := range []string{"/repo", ":fork:/repo", ":ext:user@host:/repo", ":pserver:user@host:2401/repo", ":ext:user@[::1]:/repo"} {
		if got, err := sourceDirectory(root); err != nil || got != "/repo" {
			t.Fatalf("locator %q: %q %v", root, got, err)
		}
	}
	for _, root := range []string{":pserver:user:secret@host:/repo", ":ext:user@host:invalid/repo", "/repo/../escape", ":fork:relative"} {
		if _, err := sourceDirectory(root); err == nil {
			t.Fatal("unsafe locator accepted")
		}
	}
	for _, path := range []string{"../escape", "/absolute", "a/.git/config", "a\nnewline", "a\ttab", "a\x00nul"} {
		if err := validatePath(path); err == nil {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
	for _, path := range []string{"a space", "-leading-option", "caf\xc3\xa9", "nonutf8-\xff"} {
		if err := validatePath(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExistingBareAndBinaryProvenance(t *testing.T) {
	ctx := context.Background()
	runner := &process.Runner{Log: process.DiscardLogger(), Timeout: time.Minute, MaxOutput: 16 << 20}
	dest := filepath.Join(t.TempDir(), "mirror.git")
	cmd := exec.Command("git", "init", "--bare", "--ref-format=files", dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	if err := Init(ctx, runner, dest, "/fixture/cvsroot", "module"); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, runner, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	blob, err := r.oid(ctx, []byte("content\n"), nil, "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.seedTree(ctx, map[string]string{"collision": blob, "collision/child": blob}); err == nil {
		t.Fatal("conflicting seed paths were accepted")
	}
	// Untrusted log text can impersonate provenance and contain non-UTF-8 or
	// NUL bytes; only the controlled header carries source identities.
	rev := &Revision{Path: "nonutf8-\xff", Number: "1.1", State: "dead", Ref: trunkRef, Author: "user\n<spoof>", Time: 123, Log: []byte("cvsgitrelay-Version: 999\n\n\xff\x00\n")}
	m := metadata(rev, &File{}, "-")
	tree, err := r.seedTree(ctx, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	oid, err := r.oid(ctx, message(r.Config.SourceID, "revision", m, rev.Log), exporterEnv(rev.Time, authorDisplay(rev.Author)), "commit-tree", tree)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.readCommit(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Log) != string(rev.Log) || c.Meta.PathHex != hexBytes(rev.Path) || c.Meta.AuthorHex != hexBytes(rev.Author) {
		t.Fatal("source bytes were not reversible")
	}
	if err := r.checkTransition(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Simulate an external ref creation after startup. The failed transaction
	// must leave the ownership checkpoint unchanged, with no partial advance.
	checkpoint := r.StateOID
	if _, err := r.git(ctx, nil, nil, "update-ref", trunkRef, oid, zeroOID); err != nil {
		t.Fatal(err)
	}
	if err := r.publish(ctx, trunkRef, oid, ""); err == nil {
		t.Fatal("CAS conflict was ignored")
	}
	refs, err := r.allRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if refs[stateRef] != checkpoint {
		t.Fatal("CAS failure partially advanced checkpoint")
	}
	if _, err := r.git(ctx, nil, nil, "update-ref", "-d", trunkRef, oid); err != nil {
		t.Fatal(err)
	}
	// Source settings are pinned by the Git checkpoint, even if someone edits
	// the private JSON configuration without changing its permissions.
	r.Close()
	data, err := os.ReadFile(filepath.Join(dest, "cvsgitrelay.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "cvsgitrelay.json"), []byte(replaceModule(data)), 0600); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(ctx, runner, dest); err == nil {
		other.Close()
		t.Fatal("modified immutable configuration was accepted")
	}
}

func replaceModule(data []byte) string {
	// Keep this edit independent of Config's marshaler to exercise on-disk
	// configuration drift, rather than constructing an invalid JSON file.
	return strings.Replace(string(data), `"module": "module"`, `"module": "other-module"`, 1)
}
