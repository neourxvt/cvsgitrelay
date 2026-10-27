package canonical

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"slices"
	"strings"
	"testing"
)

func policy() Policy {
	return Policy{Source: "fixture", Module: "module", Submodules: "include", Verify: "full", Mappings: []Mapping{{Archive: "module"}}}
}
func record(path, number, parent string, time int64, state, body string) Record {
	r := Record{Key: Key{"fixture", "module/" + path, number}, Path: path, Time: time, Author: "a\xff", Log: []byte("log\r\n\x00@\xff"), State: state, Present: true, Content: []byte(body)}
	if parent != "" {
		k := Key{r.Key.Source, r.Key.Archive, parent}
		r.Parent = &k
	}
	return r
}

func fixtureInventory() Inventory {
	return Inventory{Records: []Record{record("a", "1.1", "", 100, "live", "base\x00\r\n"), record("a", "1.3", "1.1", 99, "dead", ""), record("a", "1.1.2.1", "1.1", 101, "live", "branch"), record("z/\xff", "1.1", "", 100, "live", "")}, Symbols: []Symbol{{"module/a", "feature", "1.1.2", true}, {"module/a", "alias", "1.1.2", true}, {"module/a", "tag\xff", "1.3", false}, {"module/z/\xff", "tag\xff", "1.1", false}}}
}

func objectFixture(g Generation) []byte {
	var b strings.Builder
	for _, oid := range SortedKeys(g.Objects) {
		o := g.Objects[oid]
		payload := hex.EncodeToString(o.Data)
		// A visible sentinel distinguishes an empty payload without relying on
		// trailing whitespace in the fixture's printable representation.
		if payload == "" {
			payload = "-"
		}
		fmt.Fprintf(&b, "%s %s %s\n", oid, o.Kind, payload)
	}
	return []byte(b.String())
}

func TestV2Golden(t *testing.T) {
	g, e := Build(fixtureInventory(), policy())
	if e != nil {
		t.Fatal(e)
	}
	// This fixture records every complete binary payload and SHA-1 envelope
	// hash, including trees, parent order, synthetic objects and the manifest.
	got := objectFixture(g)
	path := "testdata/objects-v2.golden"
	if os.Getenv("CVSGITRELAY_UPDATE_V2_GOLDEN") == "1" {
		if e := os.MkdirAll("testdata", 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, got, 0600); e != nil {
			t.Fatal(e)
		}
	}
	want, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("v2 object bytes differ from format fixture")
	}
	m, e := ParseManifest(g.Manifest.Bytes())
	if e != nil || !bytes.Equal(m.Bytes(), g.Manifest.Bytes()) {
		t.Fatal(e)
	}
	for _, o := range g.Objects {
		if o.Kind != "commit" {
			continue
		}
		_, message, ok := bytes.Cut(o.Data, []byte("\n\n"))
		if ok && bytes.HasPrefix(message, []byte("cvsgitrelay-revision-v2\n")) {
			r, e := ParseRevisionMessage(message)
			if e != nil || !bytes.Equal(RevisionMessage(r), message) {
				t.Fatalf("message: %v", e)
			}
		}
	}
}

func TestDiscoveryAndPollingConvergence(t *testing.T) {
	p := policy()
	final := fixtureInventory()
	want, e := Build(final, p)
	if e != nil {
		t.Fatal(e)
	}
	rng := rand.New(rand.NewSource(12))
	for run := 0; run < 80; run++ {
		in := Inventory{Records: slices.Clone(final.Records), Symbols: slices.Clone(final.Symbols)}
		rng.Shuffle(len(in.Records), func(i, j int) { in.Records[i], in.Records[j] = in.Records[j], in.Records[i] })
		rng.Shuffle(len(in.Symbols), func(i, j int) { in.Symbols[i], in.Symbols[j] = in.Symbols[j], in.Symbols[i] })
		// Earlier polls may contain any ancestry-complete prefix; none of their
		// objects or first-observed branch members can influence the final run.
		for _, n := range []int{1, 2, 4} {
			subset := Inventory{Records: slices.Clone(final.Records[:n])}
			if _, e := Build(subset, p); e != nil {
				t.Fatal(e)
			}
		}
		got, e := Build(in, p)
		if e != nil {
			t.Fatal(e)
		}
		if got.State != want.State || !bytes.Equal(objectFixture(got), objectFixture(want)) {
			t.Fatal("poll/discovery order affected generation")
		}
	}
}

func TestFailureBoundaries(t *testing.T) {
	base := fixtureInventory()
	p := policy()
	for _, name := range []string{"parent", "duplicate", "overlap", "collision", "partial-alias", "timestamp", "source", "cycle"} {
		t.Run(name, func(t *testing.T) {
			in := Inventory{Records: slices.Clone(base.Records), Symbols: slices.Clone(base.Symbols)}
			switch name {
			case "parent":
				k := in.Records[0].Key
				k.Number = "1.2"
				in.Records[1].Parent = &k
			case "duplicate":
				in.Records = append(in.Records, in.Records[0])
			case "overlap":
				in.Symbols = append(in.Symbols, Symbol{"module/z/\xff", "feature", "1.1.2", true})
			case "collision":
				in.Records = append(in.Records, record("z", "1.1", "", 100, "live", ""))
			case "partial-alias":
				in.Symbols = append(in.Symbols, Symbol{"module/z/\xff", "alias", "1.1.2", true})
			case "timestamp":
				in.Records[0].Time = -1
			case "source":
				in.Records[0].Key.Source = "other"
			case "cycle":
				k := in.Records[1].Key
				in.Records[0].Parent = &k
			}
			if _, e := Build(in, p); e == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	for _, bad := range []string{"01.2", "1.-1", "1.18446744073709551616", "1.+1"} {
		if _, e := Number(bad); e == nil {
			t.Fatal("bad integer accepted")
		}
	}
	if _, e := Number("1.18446744073709551615"); e != nil {
		t.Fatal(e)
	}
}

func TestMutationAndPolicyAffectState(t *testing.T) {
	p := policy()
	in := fixtureInventory()
	g, e := Build(in, p)
	if e != nil {
		t.Fatal(e)
	}
	in.Records[0].Content = []byte("changed without metadata changes")
	next, e := Build(in, p)
	if e != nil {
		t.Fatal(e)
	}
	if g.State == next.State || g.Manifest.SourceDigest == next.Manifest.SourceDigest {
		t.Fatal("content omitted from canonical state")
	}
	p.Verify = "trust-append-only"
	next, e = Build(in, p)
	if e != nil {
		t.Fatal(e)
	}
	if next.Manifest.PolicyDigest == g.Manifest.PolicyDigest {
		t.Fatal("verification policy not pinned")
	}
	in.Symbols = nil
	next, e = Build(in, policy())
	if e != nil {
		t.Fatal(e)
	}
	if next.Manifest.SourceDigest == g.Manifest.SourceDigest {
		t.Fatal("symbols omitted from source digest")
	}
}

func TestAnchorFanInAndEmpty(t *testing.T) {
	o := Objects{}
	tree, _ := o.Tree(nil)
	var tips []string
	for i := 0; i < 130; i++ {
		tips = append(tips, o.Synthetic(tree, nil, fmt.Sprintf("test-%d\n", i)))
	}
	a := o.Anchor(tips)
	slices.Reverse(tips)
	if a != o.Anchor(tips) {
		t.Fatal("anchor order")
	}
	for _, obj := range o {
		if obj.Kind == "commit" && bytes.Count(obj.Data, []byte("\nparent ")) > 64 {
			t.Fatal("unbounded fan-in")
		}
	}
	g, e := Build(Inventory{}, policy())
	if e != nil {
		t.Fatal(e)
	}
	if len(g.Manifest.Lines) != 0 || bytes.Contains(g.Objects[g.State].Data, []byte("\nparent ")) {
		t.Fatal("empty state ancestry")
	}
}

func TestNestedVendorEmptyAndDeadForkLines(t *testing.T) {
	in := Inventory{Records: []Record{
		record("a", "1.1", "", 100, "live", "root"),
		record("a", "1.1.1.1", "1.1", 99, "live", "vendor"),
		record("a", "1.1.2.1", "1.1", 98, "dead", ""),
		record("a", "1.1.2.1.4.1", "1.1.2.1", 97, "live", "nested"),
		record("b", "1.1", "", 100, "dead", ""),
		record("b", "1.1.2.1", "1.1", 98, "live", "branch"),
	}, Symbols: []Symbol{{"module/a", "VENDOR", "1.1.1", true}, {"module/a", "feature", "1.1.2", true}, {"module/b", "feature", "1.1.2", true}, {"module/a", "empty", "1.1.6", true}, {"module/a", "mixed-tag", "1.1.2.1.4.1", false}, {"module/b", "mixed-tag", "1.1", false}}}
	g, e := Build(in, policy())
	if e != nil {
		t.Fatal(e)
	}
	if len(g.Manifest.Revisions) != 6 || len(g.Manifest.Lines) != 5 {
		t.Fatal("line/revision coverage")
	}
	if e := g.ValidateReachability(); e != nil {
		t.Fatal(e)
	}
	// Naming an unnamed numeric line can regroup history, and a fresh rebuild
	// must account for the same nested revisions without observation-time IDs.
	in.Symbols = append(in.Symbols, Symbol{"module/a", "nested", "1.1.2.1.4", true})
	named, e := Build(in, policy())
	if e != nil {
		t.Fatal(e)
	}
	if named.State == g.State {
		t.Fatal("new symbol ignored")
	}
	for _, obj := range named.Objects {
		if obj.Kind == "commit" && bytes.Contains(obj.Data, []byte("cvsgitrelay-seed-v2\n")) && bytes.Contains(obj.Data, []byte("line="+Hex("named\x00empty"))) {
			if !bytes.Contains(obj.Data, []byte("\nparent ")) {
				t.Fatal("empty branch seed unanchored")
			}
			return
		}
	}
	t.Fatal("empty branch missing")
}

func TestGeneratedEvolutionSchedules(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for run := 0; run < 60; run++ {
		var in Inventory
		for file := 0; file < 1+rng.Intn(5); file++ {
			path := fmt.Sprintf("p%d", file)
			previous := ""
			for revision := 1; revision <= 1+rng.Intn(5); revision++ {
				number := fmt.Sprintf("1.%d", revision)
				state := "live"
				if rng.Intn(3) == 0 {
					state = "dead"
				}
				in.Records = append(in.Records, record(path, number, previous, int64(100+rng.Intn(3)), state, fmt.Sprint(rng.Intn(4))))
				previous = number
				// Every intermediate state is ancestry-complete. Poll boundaries
				// may introduce another archive with an older timestamp later.
				if _, e := Build(in, policy()); e != nil {
					t.Fatal(e)
				}
			}
		}
		want, e := Build(in, policy())
		if e != nil {
			t.Fatal(e)
		}
		rng.Shuffle(len(in.Records), func(i, j int) { in.Records[i], in.Records[j] = in.Records[j], in.Records[i] })
		got, e := Build(in, policy())
		if e != nil || got.State != want.State || !bytes.Equal(objectFixture(got), objectFixture(want)) {
			t.Fatal("generated evolution did not converge", e)
		}
	}
}

func TestManifestAndProvenanceFailClosed(t *testing.T) {
	g, e := Build(fixtureInventory(), policy())
	if e != nil {
		t.Fatal(e)
	}
	manifest := string(g.Manifest.Bytes())
	for _, s := range []string{manifest + "\n", strings.Replace(manifest, "format=2", "format=02", 1), strings.Replace(manifest, "revision-count=4", "revision-count=5", 1), strings.Replace(manifest, "verify=full", "verify=unknown", 1), strings.Replace(manifest, "line-count=", "optional=x\nline-count=", 1)} {
		if _, e := ParseManifest([]byte(s)); e == nil {
			t.Fatal("noncanonical manifest accepted")
		}
	}
	message := string(RevisionMessage(fixtureInventory().Records[0]))
	for _, s := range []string{message + "\n", strings.Replace(message, "timestamp=100", "timestamp=0100", 1), strings.Replace(message, "state=live", "state=Exp", 1), message + "extra=x\n", strings.Replace(message, "archive=", "unknown=", 1)} {
		if _, e := ParseRevisionMessage([]byte(s)); e == nil {
			t.Fatal("noncanonical provenance accepted")
		}
	}
}

func BenchmarkFullReplay(b *testing.B) {
	p := policy()
	in := Inventory{}
	for i := 0; i < 100; i++ {
		in.Records = append(in.Records, record(fmt.Sprintf("f%03d", i), "1.1", "", 100, "live", strings.Repeat("x", 1024)))
	}
	b.ReportAllocs()
	b.SetBytes(100 * 1024)
	for i := 0; i < b.N; i++ {
		if _, e := Build(in, p); e != nil {
			b.Fatal(e)
		}
	}
}

// The incremental replay tree must emit exactly the trees, and tree objects,
// that the reference encoder produces from the complete file map.
func TestLineTreeMatchesReferenceTree(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	paths := []string{"a", "b", "d/a", "d/b", "d/e", "d/e/a", "d/e/f/g", "d-x", "d.x", "e/a", "e/b/c", "z"}
	files := map[string]string{}
	incremental, reference := Objects{}, Objects{}
	tree := newLineTree(incremental)
	for step := 0; step < 2000; step++ {
		path := paths[random.Intn(len(paths))]
		if random.Intn(3) == 0 {
			delete(files, path)
			tree.remove(path)
		} else {
			blob := OID("blob", []byte{byte(random.Intn(4))})
			// Skip moves that the reference would reject as file/directory conflicts.
			candidate := maps.Clone(files)
			candidate[path] = blob
			if _, err := (Objects{}).Tree(candidate); err != nil {
				if tree.set(path, blob) == nil {
					t.Fatalf("step %d: conflict accepted for %s", step, path)
				}
				continue
			}
			files = candidate
			if err := tree.set(path, blob); err != nil {
				t.Fatal(err)
			}
		}
		want, err := reference.Tree(files)
		if err != nil {
			t.Fatal(err)
		}
		if got := tree.oid(); got != want {
			t.Fatalf("step %d: tree %s, want %s", step, got, want)
		}
	}
	for oid, o := range incremental {
		if r, ok := reference[oid]; !ok || r.Kind != o.Kind || !bytes.Equal(r.Data, o.Data) {
			t.Fatalf("incremental tree emitted an object the reference never produced: %s", oid)
		}
	}
}

// Streaming hashes must keep the documented framed-buffer definitions.
func TestStreamingHashesMatchFramedBytes(t *testing.T) {
	in := fixtureInventory()
	for _, r := range in.Records {
		if r.Fingerprint() != Hash("cvsgitrelay-revision-record-v2", r.Bytes()) {
			t.Fatal("streamed fingerprint differs from framed record bytes")
		}
	}
	b := U(uint64(len(in.Records)))
	for _, r := range in.Records {
		b = append(b, B(r.Key.Bytes())...)
		b = append(b, B(r.Bytes())...)
	}
	b = append(b, U(uint64(len(in.Symbols)))...)
	for _, s := range in.Symbols {
		b = append(b, B(s.Bytes("test"))...)
	}
	if in.Digest("test") != Hash("cvsgitrelay-source-v2", b) {
		t.Fatal("streamed source digest differs from framed inventory bytes")
	}
}
