package canonical

import (
	"bytes"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func policy3() Policy {
	p := policy()
	p.Format = 3
	p.Authors = map[string]string{"bob": "Bob Builder <bob@example.com>"}
	return p
}

// The format 3 fixture covers a mapped author, a plain username, a username
// that needs the hex fallback, and verbatim, empty and substituted logs.
func fixtureInventory3() Inventory {
	in := fixtureInventory()
	in.Records[0].Author, in.Records[0].Log = "alice", []byte("Fix the build\n\nLonger explanation.\n")
	in.Records[1].Author, in.Records[1].Log = "bob", []byte("*** empty log message ***\n")
	in.Records[3].Author, in.Records[3].Log = "carol", []byte("")
	return in
}

func TestV3Golden(t *testing.T) {
	g, e := Build(fixtureInventory3(), policy3())
	if e != nil {
		t.Fatal(e)
	}
	got := objectFixture(g)
	path := "testdata/objects-v3.golden"
	if os.Getenv("CVSGITRELAY_UPDATE_V3_GOLDEN") == "1" {
		if e := os.WriteFile(path, got, 0600); e != nil {
			t.Fatal(e)
		}
	}
	want, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("v3 object bytes differ from format fixture")
	}
}

// parseV3Event recovers an event from the pieces Git stores: the author line
// of the commit header and its message.
func parseV3Event(p Policy, commit []byte) (Record, []byte, error) {
	head, message, _ := bytes.Cut(commit, []byte("\n\n"))
	lines := strings.Split(string(head), "\n")
	identity, timestamp, err := AuthorLine(lines[len(lines)-2])
	if err != nil {
		return Record{}, message, err
	}
	r, err := p.ParseEvent(identity, timestamp, message)
	return r, message, err
}

// Every event shows its log as the subject and body, its author readably when
// possible, and parses back to exactly the encoded provenance.
func TestV3EventsAreReadable(t *testing.T) {
	p := policy3()
	g, e := Build(fixtureInventory3(), p)
	if e != nil {
		t.Fatal(e)
	}
	authors := map[string]string{
		"alice": "alice <alice@cvs.invalid>",
		"bob":   "Bob Builder <bob@example.com>",
		"carol": "carol <carol@cvs.invalid>",
		"a\xff": "CVS-61ff <cvs@invalid>",
	}
	events := 0
	for _, o := range g.Objects {
		head, message, ok := bytes.Cut(o.Data, []byte("\n\n"))
		if o.Kind != "commit" || !ok || !bytes.Contains(message, []byte("\n\nCVS: ")) {
			continue
		}
		events++
		r, _, e := parseV3Event(p, o.Data)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Contains(head, []byte("\nauthor "+authors[r.Author]+" ")) {
			t.Fatalf("unexpected author for %q:\n%s", r.Author, head)
		}
		shown, _ := shownLog(r.Log)
		if !bytes.HasPrefix(message, []byte(shown)) {
			t.Fatalf("log is not the message body:\n%s", message)
		}
	}
	if events != len(fixtureInventory3().Records) {
		t.Fatalf("found %d revision events", events)
	}
}

// A typical event has a single trailer line; the others appear only when the
// header cannot carry the information.
func TestV3TrailerIsMinimal(t *testing.T) {
	p := policy3()
	r := record("dir/file", "1.29", "", 1234, "live", "")
	r.Author, r.Log = "alice", []byte("Fix the build\n")
	if got, want := string(p.RevisionMessage(r)), "Fix the build\n\nCVS: 1.29 module/dir/file\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	r.Author, r.State, r.Log = "bob", "dead", []byte("gone")
	if got, want := string(p.RevisionMessage(r)), "gone\n\nCVS: 1.29 module/dir/file\nCVS-Author: bob\nCVS-State: dead\nCVS-Log: no-newline\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	r.Log = []byte("bad \xff\n")
	if got, want := string(p.RevisionMessage(r)), "bad \uFFFD\n\nCVS: 1.29 module/dir/file\nCVS-Author: bob\nCVS-State: dead\nCVS-Log: hex:62616420ff0a\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Logs may contain anything, including text that imitates the trailer; the
// trailer is located from the end, and each message round-trips exactly.
func TestV3MessageRoundTrip(t *testing.T) {
	p := policy3()
	trailer := string(p.RevisionMessage(record("a", "1.1", "", 1, "live", "")))
	logs := []string{"", "\n", " \t\n", "subject", "subject\n", "subject\n\n\n", "line\r\nnext\r\n", "caf\xc3\xa9\n", "bad \xff byte\n", "nul\x00byte\n",
		"CVS: 1.1 module/a\n", "CVS-Log: no-newline\n", "x\n\nCVS: 1.1 module/a\n", trailer, "x\n\n" + trailer, strings.TrimSuffix(trailer, "\n"), strings.Repeat("long ", 1000)}
	paths := []string{"dir/file", "dir/my file", "hex:x/y", "dir/caf\xc3\xa9", "dir/\xff", "dir/ lead", "dir/trail ", "dir/ctl\x01", "dir/a\x7fb"}
	for _, log := range logs {
		for _, author := range []string{"alice", "bob", "x<y>", "a b"} {
			for _, path := range paths {
				for _, state := range []string{"live", "dead"} {
					r := record(path, "1.2.2.1", "", 1234, state, "")
					r.Author, r.Log = author, []byte(log)
					message := p.RevisionMessage(r)
					got, e := p.ParseEvent(p.EventAuthor(author), r.Time, message)
					if e != nil {
						t.Fatalf("log %q path %q: %v\n%s", log, path, e, message)
					}
					if got.Key != r.Key || got.Author != r.Author || !bytes.Equal(got.Log, r.Log) || got.Time != r.Time || got.State != r.State {
						t.Fatalf("log %q did not round-trip: %+v", log, got)
					}
					if bytes.IndexByte(message, 0) >= 0 || !ValidIdentity(p.EventAuthor(author)) {
						t.Fatalf("message or author cannot be stored by Git: %q", p.EventAuthor(author))
					}
				}
			}
		}
	}
}

func TestV3NoncanonicalMessagesRejected(t *testing.T) {
	p := policy3()
	r := record("a", "1.1", "", 100, "live", "")
	r.Author, r.Log = "alice", []byte("subject\n")
	identity := p.EventAuthor("alice")
	verbatim := string(p.RevisionMessage(r))
	r.Log = []byte("bad \xff\n")
	encoded := string(p.RevisionMessage(r))
	r.Author, r.Log = "bob", []byte("subject\n")
	mapped := string(p.RevisionMessage(r))
	for _, s := range []string{
		verbatim + "\n",
		strings.Replace(verbatim, "subject\n\n", "subject\n", 1),
		strings.Replace(verbatim, "CVS: 1.1", "CVS: 01.1", 1),
		strings.Replace(verbatim, "CVS: 1.1 ", "CVS: 1.1  ", 1),
		strings.Replace(verbatim, "module/a", hexPrefix+Hex("module/a"), 1),
		verbatim + "CVS-State: live\n",
		verbatim + "CVS-State: dead\nCVS-State: dead\n",
		verbatim + "CVS-Author: alice\n",
		verbatim + "CVS-Log: hex:\n",
		verbatim + "CVS-Log: 73\n",
		verbatim + "Extra: x\n",
		strings.Replace(encoded, "ff", "FF", 1),
		strings.Replace(encoded, "bad \uFFFD", "bad ?", 1),
		strings.Replace(mapped, "CVS: 1.1 module/a\nCVS-Author: bob\n", "CVS-Author: bob\nCVS: 1.1 module/a\n", 1),
	} {
		if _, e := p.ParseEvent(identity, 100, []byte(s)); e == nil {
			t.Fatalf("noncanonical provenance accepted:\n%s", s)
		}
	}
	// A mapped author must be named in the message, since the header hides it.
	if _, e := p.ParseEvent(p.EventAuthor("bob"), 100, []byte(mapped)); e != nil {
		t.Fatal(e)
	}
	if _, e := p.ParseEvent(p.EventAuthor("bob"), 100, []byte(strings.Replace(mapped, "CVS-Author: bob\n", "", 1))); e == nil {
		t.Fatal("mapped author accepted without its trailer line")
	}
	// Without an author line the Git author names the CVS user, and only the
	// canonical spelling is accepted. The time exists only in the header.
	got, e := p.ParseEvent(identity, 100, []byte(verbatim))
	if e != nil || got.Time != 100 || got.Author != "alice" {
		t.Fatalf("%+v: %v", got, e)
	}
	for _, other := range []string{p.EventAuthor("bob"), "CVS-616c696365 <cvs@invalid>", "alice <carol@cvs.invalid>", "Alice <alice@cvs.invalid>"} {
		if _, e := p.ParseEvent(other, 100, []byte(verbatim)); e == nil {
			t.Fatalf("mismatching author accepted: %q", other)
		}
	}
	// A format 2 destination never accepts a format 3 message, and vice versa.
	if _, e := policy().ParseEvent(identity, 100, []byte(verbatim)); e == nil {
		t.Fatal("format 2 accepted a format 3 message")
	}
	r.Log = []byte("subject\n")
	if _, e := p.ParseEvent(p.EventAuthor(r.Author), r.Time, policy().RevisionMessage(r)); e == nil {
		t.Fatal("format 3 accepted a format 2 message")
	}
	if _, e := p.ParseRevisionMessage([]byte(verbatim)); e == nil {
		t.Fatal("format 3 message parsed without its header")
	}
}

func TestV3PolicyAuthors(t *testing.T) {
	v2 := policy()
	v2.Authors = map[string]string{"bob": "Bob <bob@example.com>"}
	if v2.Validate() == nil {
		t.Fatal("format 2 accepted an author map")
	}
	for _, identity := range []string{"Bob", "Bob <>", "<bob@example.com>", " Bob <b@x>", "Bob <b@x> ", "B<o>b <b@x>", "Bob <b x@y>", "Bob\n <b@x>", "Bob <b@x>\x00", "Bob \xff <b@x>"} {
		p := policy3()
		p.Authors = map[string]string{"bob": identity}
		if p.Validate() == nil {
			t.Fatalf("invalid identity accepted: %q", identity)
		}
	}
	p := policy3()
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	other := policy3()
	other.Authors = map[string]string{"bob": "Robert Builder <bob@example.com>"}
	plain := policy3()
	plain.Authors = nil
	if p.Digest() == other.Digest() || p.Digest() == plain.Digest() || plain.Digest() == policy().Digest() {
		t.Fatal("author map or format does not change the policy digest")
	}
}

// commitMessage returns the message of a commit object.
func commitMessage(t *testing.T, g Generation, oid string) string {
	t.Helper()
	_, message, ok := bytes.Cut(g.Objects[oid].Data, []byte("\n\n"))
	if !ok {
		t.Fatalf("commit %s has no message", oid)
	}
	return string(message)
}

// Tags and the first commits of lines show their names instead of hex
// metadata, and tags are plain refs under refs/tags/ when the name allows it.
func TestV3TagsAndLinesAreReadable(t *testing.T) {
	in := fixtureInventory3()
	in.Symbols = append(in.Symbols, Symbol{"module/a", "REL-1_0", "1.1", false})
	p := policy3()
	g, e := Build(in, p)
	if e != nil {
		t.Fatal(e)
	}
	tag := g.Manifest.Refs["refs/tags/REL-1_0"]
	if tag == "" || commitMessage(t, g, tag) != "CVS tag REL-1_0\n" {
		t.Fatalf("plain tag not readable: %q", commitMessage(t, g, tag))
	}
	// The binary tag name keeps the hex ref and a hex marker in the message.
	raw := g.Manifest.Refs["refs/tags/cvs-raw/"+Hex("tag\xff")]
	if raw == "" || commitMessage(t, g, raw) != "CVS tag hex:"+Hex("tag\xff")+"\n" {
		t.Fatalf("raw tag not published readably: %q", commitMessage(t, g, raw))
	}
	// The line of the aliases feature and alias is named after the first.
	var messages []string
	for _, o := range g.Objects {
		if o.Kind != "commit" {
			continue
		}
		// Skip revision events (they carry a "CVS:" trailer) and the internal
		// anchor and state commits, which are never shown by Git.
		m := string(o.Data[bytes.Index(o.Data, []byte("\n\n"))+2:])
		if !strings.Contains(m, "\nCVS: ") && !strings.HasPrefix(m, "cvsgitrelay-") {
			messages = append(messages, m)
		}
	}
	for _, want := range []string{"Initial commit\n", "CVS branch alias\n"} {
		if !slices.Contains(messages, want) {
			t.Errorf("no commit with message %q in %q", want, messages)
		}
	}
	for _, m := range messages {
		if strings.Contains(m, "cvsgitrelay-") || strings.Contains(m, "sha256") {
			t.Errorf("synthetic message is not readable: %q", m)
		}
	}
	// Format 2 keeps its hex metadata and refs.
	g2, e := Build(fixtureInventory(), policy())
	if e != nil {
		t.Fatal(e)
	}
	if g2.Manifest.Refs["refs/tags/cvs/b"+Hex("tag\xff")] == "" {
		t.Fatal("format 2 tag ref changed")
	}
	// Both manifests parse back under their own policy only.
	if _, e := p.ParseManifest(g.Manifest.Bytes()); e != nil {
		t.Fatal(e)
	}
	if _, e := ParseManifest(g.Manifest.Bytes()); e == nil {
		t.Fatal("format 2 parser accepted format 3 tag refs")
	}
}

// TagRef must stay a valid, collision-free ref for every name.
func TestV3TagRefs(t *testing.T) {
	p := policy3()
	seen := map[string]string{}
	for name, want := range map[string]string{
		"rxvt-unicode-rel-9_19": "refs/tags/rxvt-unicode-rel-9_19",
		"REL1.0":                "refs/tags/REL1.0",
		"a..b":                  "refs/tags/cvs-raw/" + Hex("a..b"),
		"a.lock":                "refs/tags/cvs-raw/" + Hex("a.lock"),
		"a.":                    "refs/tags/cvs-raw/" + Hex("a."),
		"-x":                    "refs/tags/cvs-raw/" + Hex("-x"),
		"a/b":                   "refs/tags/cvs-raw/" + Hex("a/b"),
		"a b":                   "refs/tags/cvs-raw/" + Hex("a b"),
		"cvs-raw":               "refs/tags/cvs-raw/" + Hex("cvs-raw"),
		"\xff":                  "refs/tags/cvs-raw/ff",
	} {
		ref, e := p.TagRef(name)
		if e != nil || ref != want || !validTagRef(ref) {
			t.Errorf("TagRef(%q) = %q, %v; want %q", name, ref, e, want)
		}
		if other, dup := seen[ref]; dup {
			t.Errorf("%q and %q share %s", name, other, ref)
		}
		seen[ref] = name
	}
	for _, bad := range []string{"refs/tags/", "refs/tags/cvs-raw/", "refs/tags/cvs-raw/zz", "refs/tags/cvs-raw/" + Hex("plain"), "refs/tags/a..b", "refs/tags/cvs-raw", "refs/heads/x"} {
		if validTagRef(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if ref, _ := policy().TagRef("RELEASE"); ref != "refs/tags/cvs/b"+Hex("RELEASE") {
		t.Fatalf("format 2 ref %q", ref)
	}
}

// commitTime returns the committer timestamp of a commit object.
func commitTime(t *testing.T, g Generation, oid string) int64 {
	t.Helper()
	for _, line := range strings.Split(string(g.Objects[oid].Data), "\n") {
		if rest, ok := strings.CutPrefix(line, "committer "); ok {
			fields := strings.Fields(rest)
			n, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("commit %s has no committer", oid)
	return 0
}

// Format 3 dates synthetic commits from the data they snapshot: a tag by its
// newest selected revision, a branch seed by its newest fork point and the
// trunk root by its oldest revision. Format 2 keeps the fixed date.
func TestV3SyntheticCommitsAreDatedFromData(t *testing.T) {
	in := fixtureInventory3()
	// The tag selects revisions from the times 99 (dead 1.3 of a), 100 and 101.
	in.Symbols = append(in.Symbols, Symbol{"module/a", "mixed", "1.1.2.1", false}, Symbol{"module/z/\xff", "mixed", "1.1", false})
	g, e := Build(in, policy3())
	if e != nil {
		t.Fatal(e)
	}
	ref, _ := policy3().TagRef("mixed")
	if got := commitTime(t, g, g.Manifest.Refs[ref]); got != 101 {
		t.Errorf("tag dated %d, want the newest selected revision 101", got)
	}
	tagRaw, _ := policy3().TagRef("tag\xff")
	if got := commitTime(t, g, g.Manifest.Refs[tagRaw]); got != 100 {
		t.Errorf("tag dated %d, want 100", got)
	}
	for oid, o := range g.Objects {
		if o.Kind != "commit" {
			continue
		}
		switch commitMessage(t, g, oid) {
		case "Initial commit\n":
			// The oldest trunk revision is the dead 1.3, which carries time 99.
			if got := commitTime(t, g, oid); got != 99 {
				t.Errorf("trunk root dated %d, want 99", got)
			}
		case "CVS branch alias\n":
			if got := commitTime(t, g, oid); got != 100 {
				t.Errorf("branch seed dated %d, want its fork point's 100", got)
			}
		}
	}
	g2, e := Build(fixtureInventory(), policy())
	if e != nil {
		t.Fatal(e)
	}
	for oid, o := range g2.Objects {
		if o.Kind == "commit" && bytes.Contains(o.Data, []byte("\nauthor "+Identity+" ")) && commitTime(t, g2, oid) != SyntheticTime {
			t.Errorf("format 2 synthetic commit not at the fixed date: %s", oid)
		}
	}
}

// The author map entry for the relay identity signs the commits the relay
// makes: line starts and tags as author and committer, events as committer.
// Anchor and state commits keep the fixed internal identity.
func TestV3RelayIdentity(t *testing.T) {
	const bot = "Release Bot <bot@example.com>"
	p := policy3()
	p.Authors[RelayAuthor] = bot
	in := fixtureInventory3()
	g, e := Build(in, p)
	if e != nil {
		t.Fatal(e)
	}
	events := map[string]bool{}
	for _, entry := range g.Manifest.Revisions {
		events[entry.Event] = true
	}
	for oid, o := range g.Objects {
		if o.Kind != "commit" {
			continue
		}
		head, _, _ := bytes.Cut(o.Data, []byte("\n\n"))
		var author, committer string
		for _, line := range strings.Split(string(head), "\n") {
			if rest, ok := strings.CutPrefix(line, "author "); ok {
				author = rest[:strings.Index(rest, "> ")+1]
			}
			if rest, ok := strings.CutPrefix(line, "committer "); ok {
				committer = rest[:strings.Index(rest, "> ")+1]
			}
		}
		switch msg := commitMessage(t, g, oid); {
		case events[oid]:
			if committer != bot || author == bot {
				t.Errorf("event signed by %q, authored by %q", committer, author)
			}
		case strings.HasPrefix(msg, "cvsgitrelay-"):
			if author != Identity || committer != Identity {
				t.Errorf("internal commit %q signed by %q", msg, committer)
			}
		default:
			if author != bot || committer != bot {
				t.Errorf("commit %q signed by %q/%q", msg, author, committer)
			}
		}
	}
	// Without the entry the fixed identity stays, and the entry changes the policy.
	plain, e := Build(in, policy3())
	if e != nil {
		t.Fatal(e)
	}
	if plain.Manifest.PolicyDigest == g.Manifest.PolicyDigest || p.Relay() != bot || policy3().Relay() != Identity {
		t.Fatal("relay identity not part of the policy")
	}
	// A revision by a CVS user with the reserved name is ambiguous.
	in.Records[0].Author = RelayAuthor
	if _, e := Build(in, p); e == nil {
		t.Fatal("reserved username accepted")
	}
	if _, e := Build(in, policy3()); e != nil {
		t.Fatalf("reserved name only matters when mapped: %v", e)
	}
}
