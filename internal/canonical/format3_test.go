package canonical

import (
	"bytes"
	"os"
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
