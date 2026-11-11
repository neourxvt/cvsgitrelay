package canonical

import (
	"bytes"
	"strings"
	"testing"
)

func executablePolicy() Policy {
	p := policy3()
	p.Executable = true
	return p
}

// executableInventory has an executable script, a plain file and a tag on both.
func executableInventory(scriptExecutable bool) Inventory {
	script := record("script", "1.1", "", 100, "live", "#!/bin/sh\n")
	script.Executable = scriptExecutable
	script2 := record("script", "1.2", "1.1", 102, "live", "#!/bin/sh\nexit 0\n")
	script2.Executable = scriptExecutable
	return Inventory{
		Records: []Record{script, script2, record("plain", "1.1", "", 101, "live", "text\n")},
		Symbols: []Symbol{{"module/script", "v1", "1.2", false}, {"module/plain", "v1", "1.1", false}},
	}
}

// treeMode returns the mode of a root-level path in a commit's tree.
func treeMode(t *testing.T, g Generation, commit, path string) string {
	t.Helper()
	data := g.Objects[commitTree(g.Objects[commit].Data)].Data
	for len(data) > 0 {
		end := bytes.IndexByte(data, 0)
		mode, name, _ := strings.Cut(string(data[:end]), " ")
		if name == path {
			return mode
		}
		data = data[end+21:]
	}
	t.Fatalf("%s missing from the tree of %s", path, commit)
	return ""
}

// Every revision of an executable archive, and every tag snapshot holding it,
// carries mode 100755; other files stay 100644.
func TestExecutableArchivesGetMode100755(t *testing.T) {
	p := executablePolicy()
	g, err := Build(executableInventory(true), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{"1.1", "1.2"} {
		if mode := treeMode(t, g, eventOf(t, g, "script", number), "script"); mode != "100755" {
			t.Fatalf("script %s has mode %s", number, mode)
		}
	}
	if mode := treeMode(t, g, eventOf(t, g, "plain", "1.1"), "plain"); mode != "100644" {
		t.Fatalf("plain file has mode %s", mode)
	}
	tag, err := p.TagRef("v1")
	if err != nil {
		t.Fatal(err)
	}
	if mode := treeMode(t, g, g.Manifest.Refs[tag], "script"); mode != "100755" {
		t.Fatalf("tag snapshot gives the script mode %s", mode)
	}
}

// A chmod of one archive changes the fingerprints of exactly its revisions,
// so it is noticed and replayed as a change of that file's whole history.
func TestExecutableChangeAffectsOnlyThatArchive(t *testing.T) {
	p := executablePolicy()
	before, err := Build(executableInventory(false), p)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Build(executableInventory(true), p)
	if err != nil {
		t.Fatal(err)
	}
	if before.State == after.State {
		t.Fatal("a changed executable bit kept the generation")
	}
	for id, entry := range after.Manifest.Revisions {
		changed := entry.Fingerprint != before.Manifest.Revisions[id].Fingerprint
		key, _ := DecodeKey([]byte(id))
		if changed != (key.Archive == "module/script") {
			t.Fatalf("fingerprint of %s changed: %v", key.Archive, changed)
		}
	}
}

// Executable modes are a separate, format 3 only policy. Records without the
// bit keep the framed bytes they had before modes existed.
func TestExecutablePolicyBoundaries(t *testing.T) {
	p := policy()
	p.Executable = true
	if p.Validate() == nil {
		t.Fatal("format 2 accepted executable modes")
	}
	if policy3().Digest() == executablePolicy().Digest() {
		t.Fatal("executable modes kept the policy digest")
	}
	if _, err := executableInventory(true).Normalize(policy3()); err == nil {
		t.Fatal("executable record accepted outside the executable policy")
	}
	dead := record("gone", "1.1", "", 100, "dead", "")
	dead.Present, dead.Content, dead.Executable = false, nil, true
	if _, err := (Inventory{Records: []Record{dead}}).Normalize(executablePolicy()); err == nil {
		t.Fatal("executable mode accepted without present content")
	}
	plain := record("plain", "1.1", "", 101, "live", "text\n")
	if !bytes.Contains(plain.Bytes(), frame("present")) || bytes.Contains(plain.Bytes(), []byte("executable")) {
		t.Fatal("plain record bytes changed")
	}
	plain.Executable = true
	if !bytes.Contains(plain.Bytes(), frame("present-executable")) {
		t.Fatal("executable record bytes lack the executable presence")
	}
}
