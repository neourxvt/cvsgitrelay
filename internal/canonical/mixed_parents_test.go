package canonical

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// seedCommit returns the only commit with the given message.
func seedCommit(t *testing.T, g Generation, message string) string {
	t.Helper()
	found := ""
	for oid, o := range g.Objects {
		if o.Kind == "commit" && commitMessage(t, g, oid) == message {
			if found != "" {
				t.Fatalf("several commits with message %q", message)
			}
			found = oid
		}
	}
	if found == "" {
		t.Fatalf("no commit with message %q", message)
	}
	return found
}

// commitParents returns the parent OIDs in the header of a commit.
func commitParents(g Generation, oid string) []string {
	head, _, _ := bytes.Cut(g.Objects[oid].Data, []byte("\n\n"))
	var parents []string
	for _, line := range strings.Split(string(head), "\n") {
		if p, ok := strings.CutPrefix(line, "parent "); ok {
			parents = append(parents, p)
		}
	}
	return parents
}

// treeBlob returns the blob a root-level path names in a commit's tree.
func treeBlob(t *testing.T, g Generation, commit, path string) string {
	t.Helper()
	data := g.Objects[commitTree(g.Objects[commit].Data)].Data
	for len(data) > 0 {
		end := bytes.IndexByte(data, 0)
		_, name, _ := strings.Cut(string(data[:end]), " ")
		oid := data[end+1 : end+21]
		if name == path {
			return hex.EncodeToString(oid)
		}
		data = data[end+21:]
	}
	return ""
}

func eventOf(t *testing.T, g Generation, path, number string) string {
	t.Helper()
	entry, ok := g.Manifest.Revisions[(Key{"fixture", "module/" + path, number}).ID()]
	if !ok {
		t.Fatalf("no event for %s %s", path, number)
	}
	return entry.Event
}

// A module imported with cvs import, edited on trunk only in a, and branched
// from a trunk checkout forks a from trunk and b and c from the vendor line,
// which CVS's HEAD still follows for them. The branch descends from the trunk,
// even though most of its files fork from the vendor line.
func TestBranchFromTrunkAndVendorForks(t *testing.T) {
	in := Inventory{Records: []Record{
		record("a", "1.1", "", 100, "live", "a1"),
		record("a", "1.1.1.1", "1.1", 100, "live", "a1"),
		record("a", "1.2", "1.1", 110, "live", "a2"),
		record("b", "1.1", "", 100, "live", "b1"),
		record("b", "1.1.1.1", "1.1", 100, "live", "b1"),
		record("c", "1.1", "", 100, "live", "c1"),
		record("c", "1.1.1.1", "1.1", 100, "live", "c1"),
		record("a", "1.2.2.1", "1.2", 120, "live", "a on BR"),
	}, Symbols: []Symbol{
		{"module/a", "VENDOR", "1.1.1", true}, {"module/b", "VENDOR", "1.1.1", true}, {"module/c", "VENDOR", "1.1.1", true},
		{"module/a", "BR", "1.2.2", true}, {"module/b", "BR", "1.1.1.1.2", true}, {"module/c", "BR", "1.1.1.1.2", true},
	}}
	if _, e := Build(in, policy()); e != nil {
		t.Fatalf("format 2: %v", e)
	}
	g, e := Build(in, policy3())
	if e != nil {
		t.Fatal(e)
	}
	seed := seedCommit(t, g, "CVS branch BR\n")
	if got, want := commitParents(g, seed), eventOf(t, g, "a", "1.2"); len(got) != 1 || got[0] != want {
		t.Fatalf("branch seed parents %v, want the trunk event of a 1.2 %s", got, want)
	}
	// The seed tree holds every file's exact fork contents.
	for path, body := range map[string]string{"a": "a2", "b": "b1", "c": "c1"} {
		if got := treeBlob(t, g, seed, path); got != OID("blob", []byte(body)) {
			t.Errorf("seed has %s as %s, want the fork contents %q", path, got, body)
		}
	}
	if e := g.ValidateReachability(); e != nil {
		t.Fatal(e)
	}
}

// A branch made from a checkout of another branch forks the files changed on
// that branch from it, and every other file from the trunk. The new branch
// descends from the branch it was made from.
func TestBranchOfBranchWithUnchangedFiles(t *testing.T) {
	in := Inventory{Records: []Record{
		record("a", "1.1", "", 100, "live", "a1"),
		record("b", "1.1", "", 100, "live", "b1"),
		record("a", "1.1.2.1", "1.1", 110, "live", "a on BR"),
		record("a", "1.1.2.1.2.1", "1.1.2.1", 120, "live", "a on SUB"),
	}, Symbols: []Symbol{
		{"module/a", "BR", "1.1.2", true}, {"module/b", "BR", "1.1.2", true},
		{"module/a", "SUB", "1.1.2.1.2", true}, {"module/b", "SUB", "1.1.4", true},
	}}
	if _, e := Build(in, policy()); e != nil {
		t.Fatalf("format 2: %v", e)
	}
	g, e := Build(in, policy3())
	if e != nil {
		t.Fatal(e)
	}
	seed := seedCommit(t, g, "CVS branch SUB\n")
	if got, want := commitParents(g, seed), eventOf(t, g, "a", "1.1.2.1"); len(got) != 1 || got[0] != want {
		t.Fatalf("nested seed parents %v, want the BR event of a 1.1.2.1 %s", got, want)
	}
	for path, body := range map[string]string{"a": "a on BR", "b": "b1"} {
		if got := treeBlob(t, g, seed, path); got != OID("blob", []byte(body)) {
			t.Errorf("seed has %s as %s, want the fork contents %q", path, got, body)
		}
	}
	// The choice depends only on the source, never on the symbol order.
	in.Symbols[0], in.Symbols[3] = in.Symbols[3], in.Symbols[0]
	again, e := Build(in, policy3())
	if e != nil || again.State != g.State {
		t.Fatalf("symbol order changed the generation: %v", e)
	}
}

// Adding a file on a branch gives it a dead trunk 1.1 as its fork point. That
// adds nothing to the seed tree and must not move the seed date, or the whole
// branch would be rewritten. A seed with only dead fork points takes the
// oldest one, which later additions cannot move either.
func TestAddingFileOnBranchKeepsSeed(t *testing.T) {
	in := Inventory{Records: []Record{
		record("a", "1.1", "", 100, "live", "a1"),
		record("a", "1.1.2.1", "1.1", 110, "live", "a on BR"),
	}, Symbols: []Symbol{{"module/a", "BR", "1.1.2", true}}}
	before, e := Build(in, policy3())
	if e != nil {
		t.Fatal(e)
	}
	in.Records = append(in.Records, record("d", "1.1", "", 200, "dead", ""), record("d", "1.1.2.1", "1.1", 200, "live", "d on BR"))
	in.Symbols = append(in.Symbols, Symbol{"module/d", "BR", "1.1.2", true})
	after, e := Build(in, policy3())
	if e != nil {
		t.Fatal(e)
	}
	if a, b := seedCommit(t, before, "CVS branch BR\n"), seedCommit(t, after, "CVS branch BR\n"); a != b {
		t.Fatal("adding a file on the branch changed its seed commit")
	}
	ref, _, _ := policy3().BranchRef("BR")
	old, tip := before.Manifest.Refs[ref], after.Manifest.Refs[ref]
	for c := tip; c != old; {
		parents := commitParents(after, c)
		if len(parents) != 1 {
			t.Fatal("old branch tip is not an ancestor of the new one")
		}
		c = parents[0]
	}

	only := Inventory{Records: []Record{
		record("a", "1.1", "", 100, "live", "a1"),
		record("d", "1.1", "", 150, "dead", ""), record("d", "1.1.2.1", "1.1", 150, "live", "d"),
		record("e", "1.1", "", 160, "dead", ""), record("e", "1.1.2.1", "1.1", 160, "live", "e"),
	}, Symbols: []Symbol{{"module/d", "NEW", "1.1.2", true}, {"module/e", "NEW", "1.1.2", true}}}
	g, e := Build(only, policy3())
	if e != nil {
		t.Fatal(e)
	}
	if got := commitTime(t, g, seedCommit(t, g, "CVS branch NEW\n")); got != 150 {
		t.Errorf("seed of dead fork points dated %d, want the oldest 150", got)
	}
}
