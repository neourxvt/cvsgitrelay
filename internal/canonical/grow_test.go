package canonical

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// growingPolicy is the policy of a new destination.
func growingPolicy() Policy {
	p := policy3()
	p.Growing, p.Executable, p.ReadableBranches = true, true, true
	return p
}

// grev is a revision by alice with the log "change".
func grev(path, number, parent string, time int64, body string) Record {
	r := record(path, number, parent, time, "live", body)
	r.Author, r.Log = "alice", []byte("change\n")
	return r
}

// dead marks a revision dead.
func dead(r Record) Record {
	r.State, r.Present, r.Content = "dead", false, nil
	return r
}

// growingInventory has a file a with two trunk revisions and a branch, and a
// file b committed together with a's first revision.
func growingInventory() Inventory {
	return Inventory{
		Records: []Record{
			grev("a", "1.1", "", 100, "a1"), grev("a", "1.2", "1.1", 200, "a2"), grev("b", "1.1", "", 110, "b1"),
			grev("a", "1.2.2.1", "1.2", 300, "branch"),
		},
		Symbols: []Symbol{{"module/a", "feature", "1.2.2", true}, {"module/a", "REL", "1.2", false}, {"module/b", "REL", "1.1", false}},
	}
}

// growth syncs one source state after the other into a growing history, as
// a destination would, and keeps every object it ever published.
type growth struct {
	t       *testing.T
	p       Policy
	objects Objects
	g       Generation
	// history holds every generation in order.
	history []Generation
}

func newGrowth(t *testing.T, p Policy) *growth {
	return &growth{t: t, p: p, objects: Objects{}}
}

// sync grows the history with a source state and checks that no published
// ref moved backwards or disappeared.
func (h *growth) sync(in Inventory) Generation {
	h.t.Helper()
	load := func(oids []string) (Objects, error) {
		o := Objects{}
		for _, oid := range oids {
			if obj, ok := h.objects[oid]; ok {
				o[oid] = obj
			}
		}
		return o, nil
	}
	g, err := Grow(in, h.p, Published{State: h.g.State, Manifest: h.g.Manifest, Load: load})
	if err != nil {
		h.t.Fatal(err)
	}
	for oid, o := range g.Objects {
		h.objects[oid] = o
	}
	if m, err := h.p.ParseManifest(g.Manifest.Bytes()); err != nil || !bytes.Equal(m.Bytes(), g.Manifest.Bytes()) {
		h.t.Fatalf("growing manifest does not parse back: %v", err)
	}
	if h.g.State != "" {
		for ref, old := range h.g.Manifest.Refs {
			if next := g.Manifest.Refs[ref]; next == "" || !h.ancestor(old, next) {
				h.t.Fatalf("%s moved from %s to %s, which does not descend from it", ref, old, next)
			}
		}
		if !h.ancestor(h.g.State, g.State) {
			h.t.Fatal("the state does not descend from the previous state")
		}
	}
	h.g = g
	h.history = append(h.history, g)
	return g
}

// ancestor reports whether old is new or one of its ancestors.
func (h *growth) ancestor(old, new string) bool {
	seen := map[string]bool{}
	pending := []string{new}
	for len(pending) > 0 {
		oid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if oid == old {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		pending = append(pending, h.parents(oid)...)
	}
	return false
}

func (h *growth) parents(oid string) []string {
	return commitParents(Generation{Objects: h.objects}, oid)
}

// message returns a commit's message.
func (h *growth) message(oid string) string {
	_, message, _ := bytes.Cut(h.objects[oid].Data, []byte("\n\n"))
	return string(message)
}

// files returns path to blob for the tree of a commit.
func (h *growth) files(commit string) map[string]string {
	files := map[string]string{}
	var walk func(prefix, tree string)
	walk = func(prefix, tree string) {
		entries, err := treeEntries(h.objects[tree].Data)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, e := range entries {
			if e.mode == "40000" {
				walk(prefix+e.name+"/", e.oid)
			} else {
				files[prefix+e.name] = e.mode + " " + e.oid
			}
		}
	}
	walk("", commitTree(h.objects[commit].Data))
	return files
}

// checkTips requires every line of the source to have the tree that a fresh
// history of the same source has.
func (h *growth) checkTips(in Inventory) {
	h.t.Helper()
	fresh, err := Build(in, h.p)
	if err != nil {
		h.t.Fatal(err)
	}
	for ref, oid := range fresh.Manifest.Refs {
		if !h.p.IsBranchRef(ref) {
			continue
		}
		want := commitTree(fresh.Objects[oid].Data)
		if got := commitTree(h.objects[h.g.Manifest.Refs[ref]].Data); got != want {
			h.t.Fatalf("%s has tree %s, a fresh history has %s", ref, got, want)
		}
	}
}

// A fresh growing history groups the revisions of one CVS commit into one
// changeset, and a sync of an unchanged source changes nothing.
func TestGrowingHistoryStartsWithChangesets(t *testing.T) {
	h := newGrowth(t, growingPolicy())
	g := h.sync(growingInventory())
	trunk := g.Manifest.Refs["refs/heads/trunk"]
	a1, b1 := eventOf(t, g, "a", "1.1"), eventOf(t, g, "b", "1.1")
	if a1 != b1 {
		t.Fatal("revisions of one commit got separate commits")
	}
	if want := "change\n\nCVS: 1.1 module/a\nCVS: 1.1 module/b\n"; h.message(a1) != want {
		t.Fatalf("changeset message %q, want %q", h.message(a1), want)
	}
	if eventOf(t, g, "a", "1.2") != trunk || h.parents(trunk)[0] != a1 {
		t.Fatal("trunk is not root, changeset, revision")
	}
	if g.Manifest.Refs["refs/heads/feature"] != eventOf(t, g, "a", "1.2.2.1") || g.Manifest.Refs["refs/tags/REL"] != trunk {
		t.Fatal("branch or tag misplaced")
	}
	if again := h.sync(growingInventory()); again.State != g.State || len(again.Objects) != 0 {
		t.Fatal("an unchanged source changed the history")
	}
	if fresh, err := Build(growingInventory(), growingPolicy()); err != nil || fresh.State != g.State {
		t.Fatalf("Build and Grow from nothing differ: %v", err)
	}
}

// New revisions go on top of the published tips, even one CVS dates before
// the tip, and a new branch starts from its published fork point.
func TestGrowingHistoryAppends(t *testing.T) {
	h := newGrowth(t, growingPolicy())
	first := h.sync(growingInventory())
	in := growingInventory()
	late := grev("b", "1.2", "1.1", 150, "late")
	late.Log = []byte("late\n")
	in.Records = append(in.Records, late, grev("a", "1.3", "1.2", 400, "a3"))
	in.Symbols = append(in.Symbols, Symbol{"module/b", "other", "1.1.2", true})
	g := h.sync(in)
	for _, key := range []struct{ path, number string }{{"a", "1.1"}, {"a", "1.2"}, {"b", "1.1"}, {"a", "1.2.2.1"}} {
		if eventOf(t, g, key.path, key.number) != eventOf(t, first, key.path, key.number) {
			t.Fatalf("published revision %s %s got a new commit", key.path, key.number)
		}
	}
	lateEvent, a3 := eventOf(t, g, "b", "1.2"), eventOf(t, g, "a", "1.3")
	if h.parents(lateEvent)[0] != first.Manifest.Refs["refs/heads/trunk"] || h.parents(a3)[0] != lateEvent || g.Manifest.Refs["refs/heads/trunk"] != a3 {
		t.Fatal("new revisions are not appended in order")
	}
	if other := g.Manifest.Refs["refs/heads/other"]; h.parents(other)[0] != eventOf(t, g, "b", "1.1") {
		t.Fatal("new branch does not start at its fork point")
	}
	h.checkTips(in)
	if g.Corrections != 0 {
		t.Fatal("routine changes needed a correction")
	}
}

// Each change of published CVS history that canonical history answers by
// rewriting becomes a correction commit on top, and the tips match the
// source again.
func TestGrowingHistoryCorrectsChangedHistory(t *testing.T) {
	type change struct {
		name   string
		edit   func(*Inventory)
		branch bool
	}
	for _, c := range []change{
		{"head revision removed", func(in *Inventory) {
			// cvs admin -o needs the branch and tag on the revision gone first.
			in.Records = slices.DeleteFunc(in.Records, func(r Record) bool { return r.Path == "a" && r.Key.Number != "1.1" })
			in.Symbols = in.Symbols[2:]
		}, false},
		{"archive removed", func(in *Inventory) {
			in.Records = slices.DeleteFunc(in.Records, func(r Record) bool { return r.Path == "b" })
			in.Symbols = slices.DeleteFunc(in.Symbols, func(s Symbol) bool { return s.Archive == "module/b" })
		}, false},
		{"content edited", func(in *Inventory) { in.Records[1].Content = []byte("edited") }, false},
		{"archive made executable", func(in *Inventory) {
			for i := range in.Records {
				if in.Records[i].Path == "a" {
					in.Records[i].Executable = true
				}
			}
		}, true},
		{"branch extended to a file", func(in *Inventory) {
			in.Symbols = append(in.Symbols, Symbol{"module/b", "feature", "1.1.2", true})
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newGrowth(t, growingPolicy())
			first := h.sync(growingInventory())
			in := growingInventory()
			c.edit(&in)
			g := h.sync(in)
			ref := "refs/heads/trunk"
			if c.branch {
				ref = "refs/heads/feature"
			}
			tip := g.Manifest.Refs[ref]
			if g.Corrections == 0 || !strings.HasPrefix(h.message(tip), "Update files to their current CVS state\n") || h.parents(tip)[0] != first.Manifest.Refs[ref] {
				t.Fatalf("no correction on top of %s", ref)
			}
			h.checkTips(in)
			for id, entry := range first.Manifest.Revisions {
				if g.Manifest.Revisions[id].Event != entry.Event {
					t.Fatal("a published revision changed its commit")
				}
			}
			if again := h.sync(in); again.State != g.State {
				t.Fatal("the correction is not stable")
			}
		})
	}
}

// Removed revisions stay in the manifest, marked, and a log edited in CVS
// only updates the recorded fingerprint.
func TestGrowingHistoryRecordsRemovedAndEditedRevisions(t *testing.T) {
	h := newGrowth(t, growingPolicy())
	first := h.sync(growingInventory())
	in := growingInventory()
	in.Records = slices.DeleteFunc(in.Records, func(r Record) bool { return r.Path == "b" })
	in.Symbols = in.Symbols[:2]
	in.Records[0].Log = []byte("edited log\n")
	g := h.sync(in)
	b := (Key{"fixture", "module/b", "1.1"}).ID()
	a := (Key{"fixture", "module/a", "1.1"}).ID()
	if !g.Manifest.Removed[b] || g.Manifest.Revisions[b] != first.Manifest.Revisions[b] {
		t.Fatal("removed revision is not kept and marked")
	}
	if g.Manifest.Revisions[a].Event != first.Manifest.Revisions[a].Event || g.Manifest.Revisions[a].Fingerprint != in.Records[0].Fingerprint() {
		t.Fatal("edited revision kept its old fingerprint or got a new commit")
	}
	// The revision comes back without a second commit.
	back := h.sync(growingInventory())
	if back.Manifest.Removed[b] || back.Manifest.Revisions[b].Event != first.Manifest.Revisions[b].Event {
		t.Fatal("returning revision is not published again under its commit")
	}
	h.checkTips(growingInventory())
}

// A tag that CVS moved keeps its published ref and gets a new version under
// a new one, and a tag or branch CVS deleted keeps its last value.
func TestGrowingHistoryKeepsTagsAndBranches(t *testing.T) {
	h := newGrowth(t, growingPolicy())
	first := h.sync(growingInventory())
	in := growingInventory()
	in.Records = append(in.Records, grev("a", "1.3", "1.2", 400, "a3"))
	in.Symbols[1].Target = "1.3"
	in.Symbols = append(in.Symbols, Symbol{"module/a", "GONE", "1.1", false})
	g := h.sync(in)
	if g.Manifest.Refs["refs/tags/REL"] != first.Manifest.Refs["refs/tags/REL"] || g.Manifest.Refs["refs/tags/REL@2"] != eventOf(t, g, "a", "1.3") {
		t.Fatal("moved tag did not become a second version")
	}
	if h.message(g.Manifest.Refs["refs/tags/GONE"]) == "" {
		t.Fatal("new tag missing")
	}
	// Delete the tag GONE and the branch feature.
	in.Symbols = slices.DeleteFunc(in.Symbols, func(s Symbol) bool { return s.Name == "GONE" || s.Name == "feature" })
	deleted := h.sync(in)
	if deleted.Manifest.Refs["refs/tags/GONE"] != g.Manifest.Refs["refs/tags/GONE"] || deleted.Manifest.Refs["refs/heads/feature"] != g.Manifest.Refs["refs/heads/feature"] {
		t.Fatal("deleted tag or branch lost its ref")
	}
	// The same tag on the same revision again is no new version.
	in.Symbols = append(in.Symbols, Symbol{"module/a", "GONE", "1.1", false})
	if again := h.sync(in); again.Manifest.Refs["refs/tags/GONE@2"] != "" {
		t.Fatal("recreated tag with the same selection became a new version")
	}
	// CVS creates the branch again with a new branch number. Its ref moves
	// to a new line that also descends from the old branch.
	in.Records = append(in.Records, grev("a", "1.3.2.1", "1.3", 500, "new branch"))
	in.Symbols = append(in.Symbols, Symbol{"module/a", "feature", "1.3.2", true})
	recreated := h.sync(in)
	tip := recreated.Manifest.Refs["refs/heads/feature"]
	seed := h.parents(tip)[0]
	if parents := h.parents(seed); len(parents) != 2 || parents[1] != first.Manifest.Refs["refs/heads/feature"] || h.message(seed) != "CVS branch feature\n" {
		t.Fatalf("recreated branch starts at %v, not at its fork point and the old branch", parents)
	}
	if len(recreated.Manifest.Lines) != 3 {
		t.Fatalf("lines %d, want trunk, the old and the new feature line", len(recreated.Manifest.Lines))
	}
	h.checkTips(in)
}

// Revisions with a CVS commit ID form one changeset per ID, whatever their
// times; revisions without one join only within the time window, and a
// changeset never holds two revisions of one file.
func TestChangesetGrouping(t *testing.T) {
	withID := func(r Record, id string) Record { r.CommitID = id; return r }
	in := Inventory{Records: []Record{
		withID(grev("a", "1.1", "", 100, "a1"), "C1"), withID(grev("b", "1.1", "", 5000, "b1"), "C1"),
		grev("c", "1.1", "", 6000, "c1"), grev("d", "1.1", "", 6000+changesetWindow+1, "d1"),
		grev("e", "1.1", "", 7000, "e1"), grev("e", "1.2", "1.1", 7001, "e2"),
	}}
	h := newGrowth(t, growingPolicy())
	g := h.sync(in)
	if eventOf(t, g, "a", "1.1") != eventOf(t, g, "b", "1.1") {
		t.Fatal("revisions of one commit ID were split")
	}
	if eventOf(t, g, "c", "1.1") == eventOf(t, g, "d", "1.1") {
		t.Fatal("revisions beyond the window were grouped")
	}
	if eventOf(t, g, "e", "1.1") == eventOf(t, g, "e", "1.2") || h.parents(eventOf(t, g, "e", "1.2"))[0] != eventOf(t, g, "e", "1.1") {
		t.Fatal("two revisions of one file share a changeset or are out of order")
	}
	if !strings.HasSuffix(h.message(eventOf(t, g, "a", "1.1")), "CVS-Commitid: C1\n") {
		t.Fatal("commit ID trailer missing")
	}
}

// A changeset message holds every revision and parses back exactly.
func TestChangesetMessageRoundTrip(t *testing.T) {
	p := growingPolicy()
	members := []Record{
		dead(grev("z", "1.4", "1.3", 1, "")), grev("a b", "1.2", "1.1", 1, "x"),
	}
	for i := range members {
		members[i].Author, members[i].Log, members[i].CommitID = "bob", []byte("no newline"), "ID1"
	}
	message := p.changesetMessage(members)
	want := "no newline\n\nCVS: 1.2 module/a b\nCVS-Dead: 1.4 module/z\nCVS-Author: bob\nCVS-Commitid: ID1\nCVS-Log: no-newline\n"
	if string(message) != want {
		t.Fatalf("message %q, want %q", message, want)
	}
	got, err := p.ParseChangeset(p.EventAuthor("bob"), message)
	if err != nil || len(got) != 2 || got[0].Key.Archive != "module/a b" || got[1].State != "dead" || string(got[0].Log) != "no newline" || got[1].CommitID != "ID1" {
		t.Fatalf("parsed %+v, %v", got, err)
	}
	for _, bad := range []string{
		"x\n\nCVS-Dead: 1.4 module/z\nCVS: 1.2 module/a\n",
		"x\n\nCVS: 1.2 module/a\nCVS: 1.3 module/a\n",
		"x\n\nCVS-Author: bob\n",
		"x\n\nCVS: 1.2 module/a\nExtra: y\n",
	} {
		if _, err := p.ParseChangeset(p.EventAuthor("bob"), []byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// Growing history is a policy of its own, and its manifest is only accepted
// under it.
func TestGrowingPolicy(t *testing.T) {
	p := growingPolicy()
	if p.Digest() == readablePolicy().Digest() {
		t.Fatal("growing history kept the policy digest")
	}
	v2 := policy()
	v2.Growing = true
	if v2.Validate() == nil {
		t.Fatal("format 2 accepted growing history")
	}
	g, err := Build(growingInventory(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readablePolicy().ParseManifest(g.Manifest.Bytes()); err == nil {
		t.Fatal("a canonical policy accepted a growing manifest")
	}
	in := growingInventory()
	in.Records[0].CommitID = "X"
	if _, err := Build(in, readablePolicy()); err == nil {
		t.Fatal("a canonical policy accepted commit IDs")
	}
	if in.Digest("fixture") == growingInventory().Digest("fixture") {
		t.Fatal("commit IDs do not change the source digest")
	}
	if _, err := Grow(growingInventory(), readablePolicy(), Published{}); err == nil {
		t.Fatal("Grow accepted a canonical policy")
	}
}

// A branch name that CVS moves onto a line published under another name
// makes that line continue the old one with a merge, since its ref may only
// move forward.
func TestGrowingHistoryContinuesMovedBranchName(t *testing.T) {
	h := newGrowth(t, growingPolicy())
	in := growingInventory()
	in.Symbols = append(in.Symbols, Symbol{"module/b", "other", "1.1.2", true})
	first := h.sync(in)
	// As with cvs admin -n: feature leaves a and becomes an alias of other.
	in.Symbols = slices.DeleteFunc(in.Symbols, func(s Symbol) bool { return s.Name == "feature" })
	in.Symbols = append(in.Symbols, Symbol{"module/b", "feature", "1.1.2", true})
	g := h.sync(in)
	tip := g.Manifest.Refs["refs/heads/feature"]
	if tip != g.Manifest.Refs["refs/heads/other"] || h.message(tip) != "CVS branch feature continues here\n" {
		t.Fatal("moved branch name does not continue on its new line")
	}
	if parents := h.parents(tip); len(parents) != 2 || parents[0] != first.Manifest.Refs["refs/heads/other"] || parents[1] != first.Manifest.Refs["refs/heads/feature"] {
		t.Fatalf("continuation has parents %v", parents)
	}
	h.checkTips(in)
}

// Random sequences of CVS activity, including the changes that rewrite
// canonical history, only ever move refs forward and always leave the tips
// with the trees of a fresh history of the same source.
func TestGrowingHistoryRandomEvolution(t *testing.T) {
	corrections, moved := 0, 0
	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		h := newGrowth(t, growingPolicy())
		in := Inventory{}
		heads := map[string]string{}
		now := int64(1000)
		for step := 0; step < 25; step++ {
			path := []string{"f0", "f1", "d/f2", "d/f3"}[rng.Intn(4)]
			now += int64(rng.Intn(400))
			switch op := rng.Intn(10); {
			case op < 5:
				// A trunk commit, sometimes dated before earlier ones.
				head := heads[path]
				number := "1.1"
				if head != "" {
					n, _ := Number(head)
					number = "1." + strconv.FormatUint(n[1]+1, 10)
				}
				r := grev(path, number, head, now-int64(rng.Intn(2))*700, fmt.Sprint(seed, step))
				r.Log = []byte([]string{"one\n", "two\n"}[rng.Intn(2)])
				if head != "" && rng.Intn(5) == 0 {
					r = dead(r)
				}
				in.Records = append(in.Records, r)
				heads[path] = number
			case op < 7 && heads[path] != "":
				// A tag, new or moved.
				name := []string{"T1", "T2"}[rng.Intn(2)]
				in.Symbols = slices.DeleteFunc(in.Symbols, func(s Symbol) bool { return s.Name == name && s.Archive == "module/"+path })
				in.Symbols = append(in.Symbols, Symbol{"module/" + path, name, heads[path], false})
			case op < 8:
				// A chmod of the archive.
				for i := range in.Records {
					if in.Records[i].Path == path && in.Records[i].Present {
						in.Records[i].Executable = !in.Records[i].Executable
					}
				}
			case op < 9 && heads[path] != "" && heads[path] != "1.1":
				// cvs admin -o of an untagged head revision.
				head := heads[path]
				tagged := slices.ContainsFunc(in.Symbols, func(s Symbol) bool { return s.Archive == "module/"+path && s.Target == head })
				if tagged {
					break
				}
				i := slices.IndexFunc(in.Records, func(r Record) bool { return r.Path == path && r.Key.Number == head })
				heads[path] = in.Records[i].Parent.Number
				in.Records = slices.Delete(in.Records, i, i+1)
			default:
				// A tag deleted.
				name := []string{"T1", "T2"}[rng.Intn(2)]
				in.Symbols = slices.DeleteFunc(in.Symbols, func(s Symbol) bool { return s.Name == name })
			}
			// Executable bits belong to the archive, so all present records
			// of one file share them.
			exec := map[string]bool{}
			for _, r := range in.Records {
				if r.Present {
					exec[r.Path] = exec[r.Path] || r.Executable
				}
			}
			for i := range in.Records {
				in.Records[i].Executable = in.Records[i].Present && exec[in.Records[i].Path]
			}
			if len(in.Records) == 0 {
				continue
			}
			g := h.sync(in)
			h.checkTips(in)
			corrections += g.Corrections
			for ref := range g.Manifest.Refs {
				if strings.Contains(ref, "@") {
					moved++
				}
			}
		}
	}
	// The schedules must reach the interesting cases.
	if corrections == 0 || moved == 0 {
		t.Fatalf("schedules made %d corrections and %d moved tag versions", corrections, moved)
	}
	t.Logf("%d corrections, %d moved tag versions", corrections, moved)
}
