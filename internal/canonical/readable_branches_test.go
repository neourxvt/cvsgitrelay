package canonical

import (
	"testing"
)

func readablePolicy() Policy {
	p := policy3()
	p.ReadableBranches = true
	return p
}

// readableInventory gives the fixture's branch line, besides feature and
// alias, names that are reserved, unsafe or merely look like an encoding.
func readableInventory() Inventory {
	in := fixtureInventory3()
	for _, name := range []string{"trunk", "cvs-raw", "HEAD", "bad name", "b10"} {
		in.Symbols = append(in.Symbols, Symbol{"module/a", name, "1.1.2", true})
	}
	return in
}

// The trunk and safe branch names become plain refs/heads/ names. Reserved and
// unsafe names fall back to cvs-raw/<hex>, so a CVS branch named trunk never
// replaces the trunk.
func TestReadableBranchNames(t *testing.T) {
	p := readablePolicy()
	g, e := Build(readableInventory(), p)
	if e != nil {
		t.Fatal(e)
	}
	// The fixture has the trunk and one branch line carrying every name.
	trunk, branch := g.Manifest.Lines["trunk"], ""
	for id, tip := range g.Manifest.Lines {
		if id != "trunk" {
			branch = tip
		}
	}
	if trunk == "" || branch == "" || len(g.Manifest.Lines) != 2 {
		t.Fatal("fixture lacks the expected lines")
	}
	want := map[string]string{
		"refs/heads/trunk":                      trunk,
		"refs/heads/feature":                    branch,
		"refs/heads/alias":                      branch,
		"refs/heads/b10":                        branch,
		"refs/heads/cvs-raw/" + Hex("trunk"):    branch,
		"refs/heads/cvs-raw/" + Hex("cvs-raw"):  branch,
		"refs/heads/cvs-raw/" + Hex("HEAD"):     branch,
		"refs/heads/cvs-raw/" + Hex("bad name"): branch,
	}
	heads := 0
	for ref, oid := range g.Manifest.Refs {
		if p.IsBranchRef(ref) {
			heads++
			if want[ref] != oid {
				t.Fatalf("%s points to %s, want %s", ref, oid, want[ref])
			}
		}
	}
	if heads != len(want) {
		t.Fatalf("got %d branch refs, want %d: %v", heads, len(want), g.Manifest.Refs)
	}
	if p.HeadRef() != "refs/heads/trunk" {
		t.Fatalf("HEAD is %s", p.HeadRef())
	}
	// Only the projection differs: the history is the default one.
	plain, e := Build(readableInventory(), policy3())
	if e != nil {
		t.Fatal(e)
	}
	if plain.Manifest.Lines["trunk"] != trunk || len(plain.Manifest.Revisions) != len(g.Manifest.Revisions) {
		t.Fatal("readable names changed the history")
	}
	m, e := p.ParseManifest(g.Manifest.Bytes())
	if e != nil || string(m.Bytes()) != string(g.Manifest.Bytes()) {
		t.Fatalf("readable manifest not accepted: %v", e)
	}
	if _, e := policy3().ParseManifest(g.Manifest.Bytes()); e == nil {
		t.Fatal("a policy without readable names accepted the manifest")
	}
}

// Exactly the refs the naming produces parse back; an encoded name that has a
// plain form, or a noncanonical encoding, does not.
func TestReadableBranchRefsParseBackExactly(t *testing.T) {
	for ref, want := range map[string]bool{
		"refs/heads/trunk":                     true,
		"refs/heads/REL-1_0":                   true,
		"refs/heads/cvs-raw/" + Hex("trunk"):   true,
		"refs/heads/cvs-raw/" + Hex("a b"):     true,
		"refs/heads/cvs-raw/" + Hex("feature"): false,
		"refs/heads/cvs-raw/7472756E6B":        false,
		"refs/heads/cvs-raw/":                  false,
		"refs/heads/cvs-raw":                   false,
		"refs/heads/a/b":                       false,
		"refs/heads/":                          false,
		"refs/tags/trunk":                      false,
	} {
		if got := validReadableBranchRef(ref); got != want {
			t.Errorf("validReadableBranchRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

// Readable names are a format 3 policy of their own that a branch map
// replaces, and they own the whole branch namespace.
func TestReadableBranchPolicy(t *testing.T) {
	p := readablePolicy()
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	v2 := policy()
	v2.ReadableBranches = true
	if v2.Validate() == nil {
		t.Fatal("format 2 accepted readable branch names")
	}
	mapped := readablePolicy()
	mapped.Branches = map[string]string{"trunk": "main"}
	if mapped.Validate() == nil {
		t.Fatal("readable names accepted next to a branch map")
	}
	if p.Digest() == policy3().Digest() {
		t.Fatal("readable names kept the policy digest")
	}
	for ref, owned := range map[string]bool{"refs/heads/anything": true, "refs/heads/cvs/trunk": true, "refs/tags/x": false} {
		if p.IsBranchRef(ref) != owned {
			t.Errorf("IsBranchRef(%q) = %v", ref, !owned)
		}
	}
	if policy3().IsBranchRef("refs/heads/feature") || !policy3().IsBranchRef("refs/heads/cvs/trunk") {
		t.Fatal("default ownership changed")
	}
}
