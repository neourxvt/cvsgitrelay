package canonical

import (
	"os/exec"
	"strings"
	"testing"
)

func branchPolicy(branches map[string]string) Policy {
	p := policy()
	p.Branches = branches
	return p
}

// The fixture has the trunk and one branch line with the aliases feature and
// alias. A map selects, renames and drops branch refs, while lines and tags
// are untouched.
func TestBranchMapSelectsAndRenamesRefs(t *testing.T) {
	plain, e := Build(fixtureInventory(), policy())
	if e != nil {
		t.Fatal(e)
	}
	mapped, e := Build(fixtureInventory(), branchPolicy(map[string]string{"trunk": "main", "feature": "release/1.0"}))
	if e != nil {
		t.Fatal(e)
	}
	trunk, feature := plain.Manifest.Refs["refs/heads/cvs/trunk"], plain.Manifest.Refs[mustRef(t, "refs/heads/cvs/", "feature")]
	if trunk == "" || feature == "" {
		t.Fatal("fixture lacks the expected branches")
	}
	if mapped.Manifest.Refs["refs/heads/main"] != trunk || mapped.Manifest.Refs["refs/heads/release/1.0"] != feature {
		t.Fatalf("mapped branches point elsewhere: %v", mapped.Manifest.Refs)
	}
	for ref, oid := range mapped.Manifest.Refs {
		if strings.HasPrefix(ref, "refs/heads/cvs/") {
			t.Fatalf("unmapped branch ref %s published", ref)
		}
		// Everything else, the tags and lines, is identical to the default.
		if !strings.HasPrefix(ref, "refs/heads/") && plain.Manifest.Refs[ref] != oid {
			t.Fatalf("%s differs from the default projection", ref)
		}
	}
	if len(mapped.Manifest.Refs) != len(plain.Manifest.Refs)-1 {
		t.Fatalf("want one branch alias dropped: %d vs %d refs", len(mapped.Manifest.Refs), len(plain.Manifest.Refs))
	}
	// The event history does not depend on the projection.
	if len(plain.Manifest.Revisions) != len(mapped.Manifest.Revisions) {
		t.Fatal("branch map changed the revisions")
	}
	if plain.State == mapped.State || plain.Manifest.PolicyDigest == mapped.Manifest.PolicyDigest {
		t.Fatal("branch map is not part of the policy")
	}
}

// Several aliases of one line may be mapped; the trunk may be left out.
func TestBranchMapAliasesAndMissingTrunk(t *testing.T) {
	p := branchPolicy(map[string]string{"feature": "b/one", "alias": "b/two"})
	g, e := Build(fixtureInventory(), p)
	if e != nil {
		t.Fatal(e)
	}
	if g.Manifest.Refs["refs/heads/b/one"] == "" || g.Manifest.Refs["refs/heads/b/one"] != g.Manifest.Refs["refs/heads/b/two"] {
		t.Fatalf("aliases do not share the line tip: %v", g.Manifest.Refs)
	}
	if _, ok := g.Manifest.Refs["refs/heads/cvs/trunk"]; ok {
		t.Fatal("trunk published without being selected")
	}
	if got := p.HeadRef(); got != "refs/heads/b/one" {
		t.Fatalf("HEAD is %s", got)
	}
	if got := branchPolicy(map[string]string{"trunk": "main"}).HeadRef(); got != "refs/heads/main" {
		t.Fatalf("HEAD is %s", got)
	}
	if got := policy().HeadRef(); got != "refs/heads/cvs/trunk" {
		t.Fatalf("default HEAD is %s", got)
	}
}

// The map is parsed back only by the policy that owns it, and an empty map
// keeps the digest of destinations that never had one.
func TestBranchMapManifestAndDigest(t *testing.T) {
	p := branchPolicy(map[string]string{"trunk": "main"})
	g, e := Build(fixtureInventory(), p)
	if e != nil {
		t.Fatal(e)
	}
	m, e := p.ParseManifest(g.Manifest.Bytes())
	if e != nil || string(m.Bytes()) != string(g.Manifest.Bytes()) {
		t.Fatalf("mapped manifest not accepted: %v", e)
	}
	if _, e := ParseManifest(g.Manifest.Bytes()); e == nil {
		t.Fatal("default parser accepted a mapped ref")
	}
	if _, e := branchPolicy(map[string]string{"trunk": "other"}).ParseManifest(g.Manifest.Bytes()); e == nil {
		t.Fatal("a different map accepted the manifest")
	}
	if policy().Digest() != branchPolicy(map[string]string{}).Digest() {
		t.Fatal("an empty map changed the digest")
	}
	if p.Digest() == branchPolicy(map[string]string{"trunk": "other"}).Digest() {
		t.Fatal("digest ignores branch names")
	}
}

// A real branch called trunk cannot be told apart from the trunk in the map.
func TestBranchMapRejectsBranchNamedTrunk(t *testing.T) {
	in := fixtureInventory()
	in.Symbols = append(in.Symbols, Symbol{"module/a", "trunk", "1.1.2", true})
	if _, e := Build(in, policy()); e != nil {
		t.Fatalf("default projection must keep working: %v", e)
	}
	if _, e := Build(in, branchPolicy(map[string]string{"trunk": "main"})); e == nil {
		t.Fatal("ambiguous branch name accepted")
	}
}

func TestBranchMapValidation(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"duplicate target": {"a": "x", "b": "x"},
		"directory clash":  {"a": "x", "b": "x/y"},
		"invalid target":   {"a": "bad name"},
		"empty target":     {"a": ""},
		"empty source":     {"": "x"},
	} {
		if e := ValidateBranchMap(m); e == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if e := ValidateBranchMap(map[string]string{"trunk": "main", "REL-1_0": "release/1.0", "x": "release-x"}); e != nil {
		t.Fatal(e)
	}
}

// ValidBranchName follows git check-ref-format, which is the authority when
// Git is installed.
func TestValidBranchNameMatchesGit(t *testing.T) {
	git, e := exec.LookPath("git")
	if e != nil {
		t.Skip("git not installed")
	}
	for _, name := range []string{"main", "a/b", "release/1.0", "a.b", "a-b", "é", "-a", "/a", "a/", "a//b", "a..b", "a.", ".a", "a/.b", "a.lock", "a.lock/b", "a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", "a@{b", "@", "a\x01b", "a\x7fb", "", "a@b", "a{b"} {
		// git check-ref-format --branch also rejects leading dashes and "@".
		want := exec.Command(git, "check-ref-format", "refs/heads/"+name).Run() == nil && !strings.HasPrefix(name, "-") && name != "@"
		if got := ValidBranchName(name); got != want {
			t.Errorf("ValidBranchName(%q) = %v, git says %v", name, got, want)
		}
	}
}

func mustRef(t *testing.T, prefix, name string) string {
	t.Helper()
	ref, e := EncodedRef(prefix, name)
	if e != nil {
		t.Fatal(e)
	}
	return ref
}
