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
		if o.Kind != "commit" || !ok || !bytes.Contains(message, []byte("\nCvsgitrelay-Revision: v3\n")) {
			continue
		}
		events++
		r, e := p.ParseRevisionMessage(message)
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

// Logs may contain anything, including text that imitates the trailer; the
// trailer is located from the end, and each message round-trips exactly.
func TestV3MessageRoundTrip(t *testing.T) {
	p := policy3()
	trailer := string(p.RevisionMessage(record("a", "1.1", "", 1, "live", "")))
	logs := []string{"", "\n", " \t\n", "subject", "subject\n", "subject\n\n\n", "line\r\nnext\r\n", "caf\xc3\xa9\n", "bad \xff byte\n", "nul\x00byte\n",
		"Cvsgitrelay-Log-Length: 3\n", trailer, "x\n\n" + trailer, strings.Repeat("long ", 1000)}
	for _, log := range logs {
		for _, author := range []string{"alice", "bob", "x<y>", "a b"} {
			r := record("dir/file", "1.2.2.1", "", 1234, "dead", "")
			r.Author, r.Log = author, []byte(log)
			message := p.RevisionMessage(r)
			got, e := p.ParseRevisionMessage(message)
			if e != nil {
				t.Fatalf("log %q: %v\n%s", log, e, message)
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

func TestV3NoncanonicalMessagesRejected(t *testing.T) {
	p := policy3()
	r := record("a", "1.1", "", 100, "live", "")
	r.Author, r.Log = "alice", []byte("subject\n")
	verbatim := string(p.RevisionMessage(r))
	r.Log = []byte("bad \xff\n")
	encoded := string(p.RevisionMessage(r))
	for _, s := range []string{
		verbatim + "\n",
		strings.Replace(verbatim, "subject\n\n", "subject\n", 1),
		strings.Replace(verbatim, "Log-Length: 8", "Log-Length: 08", 1),
		strings.Replace(verbatim, "Log-Length: 8", "Log: 7375626a6563740a", 1),
		strings.Replace(verbatim, "Revision: v3", "Revision: v2", 1),
		strings.Replace(verbatim, "Timestamp: 100", "Timestamp: 0100", 1),
		strings.Replace(verbatim, "State: live", "State: Exp", 1),
		strings.Replace(verbatim, "Cvsgitrelay-Author:", "Cvsgitrelay-Author: \nX:", 1),
		strings.Replace(encoded, "ff", "FF", 1),
		strings.Replace(encoded, "bad \uFFFD", "bad ?", 1),
	} {
		if _, e := p.ParseRevisionMessage([]byte(s)); e == nil {
			t.Fatalf("noncanonical provenance accepted:\n%s", s)
		}
	}
	// A format 2 destination never accepts a format 3 message, and vice versa.
	if _, e := policy().ParseRevisionMessage([]byte(verbatim)); e == nil {
		t.Fatal("format 2 accepted a format 3 message")
	}
	if _, e := p.ParseRevisionMessage(policy().RevisionMessage(r)); e == nil {
		t.Fatal("format 3 accepted a format 2 message")
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
