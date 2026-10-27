package canonical

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Object struct {
	Kind string
	Data []byte
}
type Objects map[string]Object

// Git's object envelope is part of the hash, but not the stored payload.
func OID(kind string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}
func (o Objects) Put(kind string, data []byte) string {
	id := OID(kind, data)
	o[id] = Object{kind, slices.Clone(data)}
	return id
}

// putShared stores data without copying. Callers guarantee that the slice is
// never modified afterwards, such as normalized inventory contents.
func (o Objects) putShared(kind string, data []byte) string {
	id := OID(kind, data)
	o[id] = Object{kind, data}
	return id
}
func ValidOID(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 20 && hex.EncodeToString(b) == s
}

// Tree emits the raw recursive Git representation. Directory names sort with
// a virtual slash, rather than the NUL used to terminate their encoded names.
func (o Objects) Tree(files map[string]string) (string, error) {
	type node struct {
		files map[string]string
		dirs  map[string]*node
	}
	root := &node{map[string]string{}, map[string]*node{}}
	for path, oid := range files {
		if err := Path(path); err != nil {
			return "", err
		}
		if !ValidOID(oid) {
			return "", fmt.Errorf("invalid tree blob")
		}
		n := root
		parts := strings.Split(path, "/")
		for _, part := range parts[:len(parts)-1] {
			if n.files[part] != "" {
				return "", fmt.Errorf("file/directory conflict")
			}
			if n.dirs[part] == nil {
				n.dirs[part] = &node{map[string]string{}, map[string]*node{}}
			}
			n = n.dirs[part]
		}
		base := parts[len(parts)-1]
		if n.dirs[base] != nil {
			return "", fmt.Errorf("directory/file conflict")
		}
		n.files[base] = oid
	}
	var emit func(*node) string
	emit = func(n *node) string {
		names := make([]string, 0, len(n.files)+len(n.dirs))
		for s := range n.files {
			names = append(names, s)
		}
		for s := range n.dirs {
			names = append(names, s+"/")
		}
		slices.Sort(names)
		var b []byte
		for _, name := range names {
			mode := "100644"
			oid := n.files[name]
			if strings.HasSuffix(name, "/") {
				mode = "40000"
				name = strings.TrimSuffix(name, "/")
				oid = emit(n.dirs[name])
			}
			b = append(b, []byte(mode+" "+name+"\x00")...)
			raw, _ := hex.DecodeString(oid)
			b = append(b, raw...)
		}
		return o.Put("tree", b)
	}
	return emit(root), nil
}

func CommitBytes(tree string, parents []string, author string, timestamp int64, message []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s %d +0000\ncommitter %s %d +0000\n\n", author, timestamp, Identity, timestamp)
	b.Write(message)
	return b.Bytes()
}
func (o Objects) Synthetic(tree string, parents []string, message string) string {
	return o.Put("commit", CommitBytes(tree, parents, Identity, SyntheticTime, []byte(message)))
}

// RevisionMessage encodes a revision event message in the policy's format.
func (p Policy) RevisionMessage(r Record) []byte {
	if p.Version() == 3 {
		return revisionMessageV3(r)
	}
	return revisionMessageV2(r)
}

func revisionMessageV2(r Record) []byte {
	return []byte(fmt.Sprintf("cvsgitrelay-revision-v2\nsource=%s\narchive=%s\nrevision=%s\nstate=%s\ntimestamp=%d\nauthor=%s\nlog=%s\n", Hex(r.Key.Source), Hex(r.Key.Archive), r.Key.Number, r.State, r.Time, Hex(r.Author), hex.EncodeToString(r.Log)))
}

// Format 3 trailer lines, in order. The last line carries the log, either as
// the byte length of the verbatim body or as hex when the body is substituted.
const (
	trailerPrefix    = "Cvsgitrelay-"
	trailerLogLength = trailerPrefix + "Log-Length: "
	trailerLogHex    = trailerPrefix + "Log: "
)

var trailerNames = []string{"Revision", "Source", "Archive", "CVS-Revision", "State", "Timestamp", "Author"}

// EmptyLogText replaces a log without visible text, so a commit never lacks
// a subject line.
const EmptyLogText = "(empty CVS log message)\n"

// shownLog returns the message body. A valid UTF-8 log without NUL bytes and
// with visible text is shown verbatim; anything else is shown as readable
// substitute text, and the trailer then carries the exact bytes as hex.
func shownLog(log []byte) (string, bool) {
	if utf8.Valid(log) && bytes.IndexByte(log, 0) < 0 {
		if len(bytes.TrimSpace(log)) == 0 {
			return EmptyLogText, false
		}
		return string(log), true
	}
	return strings.ReplaceAll(strings.ToValidUTF8(string(log), "\uFFFD"), "\x00", "\uFFFD"), false
}

// revisionMessageV3 puts the CVS log first, so Git shows it as the subject and
// body, then a blank line and a Git trailer block with the exact provenance.
func revisionMessageV3(r Record) []byte {
	shown, verbatim := shownLog(r.Log)
	var b bytes.Buffer
	b.WriteString(shown)
	if !strings.HasSuffix(shown, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	values := []string{"v3", Hex(r.Key.Source), Hex(r.Key.Archive), r.Key.Number, r.State, strconv.FormatInt(r.Time, 10), Hex(r.Author)}
	for i, name := range trailerNames {
		fmt.Fprintf(&b, "%s%s: %s\n", trailerPrefix, name, values[i])
	}
	if verbatim {
		fmt.Fprintf(&b, "%s%d\n", trailerLogLength, len(r.Log))
	} else {
		fmt.Fprintf(&b, "%s%s\n", trailerLogHex, hex.EncodeToString(r.Log))
	}
	return b.Bytes()
}

// safeUsername limits plain identities to bytes that need no escaping in Git
// identities or in the synthesized e-mail local part.
func safeUsername(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}

// ValidIdentity accepts "Name <email>" with a nonempty name and e-mail that
// git fsck accepts: no angle brackets, line breaks or NUL bytes inside either,
// no surrounding spaces, and valid UTF-8.
func ValidIdentity(identity string) bool {
	name, rest, ok := strings.Cut(identity, " <")
	email, ok2 := strings.CutSuffix(rest, ">")
	if !ok || !ok2 || !utf8.ValidString(identity) || name == "" || email == "" {
		return false
	}
	return !strings.ContainsAny(name, "<>\n\x00") && !strings.ContainsAny(email, "<> \n\x00") && strings.TrimSpace(name) == name
}

// EventAuthor returns the Git author of a revision event. Format 3 uses the
// author map, then the plain username; usernames that cannot be represented
// safely keep the format 2 hex identity.
func (p Policy) EventAuthor(author string) string {
	if p.Version() == 3 {
		if identity, ok := p.Authors[author]; ok {
			return identity
		}
		if safeUsername(author) {
			return author + " <" + author + "@cvs.invalid>"
		}
	}
	return "CVS-" + Hex(author) + " <cvs@invalid>"
}

func (o Objects) Event(p Policy, r Record, tree, parent string) string {
	return o.Put("commit", CommitBytes(tree, []string{parent}, p.EventAuthor(r.Author), r.Time, p.RevisionMessage(r)))
}

// Anchor bounds fan-in without allowing input discovery order into parent lists.
func (o Objects) Anchor(tips []string) string {
	tips = slices.Clone(tips)
	empty, _ := o.Tree(nil)
	for {
		slices.Sort(tips)
		tips = slices.Compact(tips)
		if len(tips) == 0 {
			return ""
		}
		if len(tips) == 1 {
			return tips[0]
		}
		var next []string
		for start := 0; start < len(tips); start += 64 {
			group := tips[start:min(start+64, len(tips))]
			if len(group) == 1 {
				next = append(next, group[0])
			} else {
				next = append(next, o.Synthetic(empty, group, "cvsgitrelay-anchor-v2\n"))
			}
		}
		tips = next
	}
}

// Revision provenance is strict: round-tripping rejects extra fields, alternate
// hex/number spellings, and unrecognized versions or message boundaries.
func (p Policy) ParseRevisionMessage(b []byte) (Record, error) {
	var r Record
	var err error
	if p.Version() == 3 {
		r, err = parseRevisionFieldsV3(b)
	} else {
		r, err = parseRevisionFieldsV2(b)
	}
	if err != nil {
		return r, err
	}
	if err := validRevisionIdentity(r); err != nil {
		return r, err
	}
	if !bytes.Equal(p.RevisionMessage(r), b) || (r.State != "live" && r.State != "dead") {
		return r, fmt.Errorf("noncanonical revision message")
	}
	return r, nil
}

func parseRevisionFieldsV2(b []byte) (Record, error) {
	f := strings.Split(string(b), "\n")
	if len(f) != 9 || f[0] != "cvsgitrelay-revision-v2" || f[8] != "" {
		return Record{}, fmt.Errorf("invalid revision message")
	}
	names := []string{"source", "archive", "revision", "state", "timestamp", "author", "log"}
	values := map[string]string{}
	for i, n := range names {
		v, ok := strings.CutPrefix(f[i+1], n+"=")
		if !ok {
			return Record{}, fmt.Errorf("invalid field order")
		}
		values[n] = v
	}
	log, err := hex.DecodeString(values["log"])
	if err != nil {
		return Record{}, err
	}
	return decodeRevisionFields(values["source"], values["archive"], values["revision"], values["state"], values["timestamp"], values["author"], log)
}

// The trailer has a fixed number of lines at the end of the message, so it is
// found from the end; everything before it is message body, whatever it says.
func parseRevisionFieldsV3(b []byte) (Record, error) {
	s, ok := strings.CutSuffix(string(b), "\n")
	lines := strings.Split(s, "\n")
	if !ok || len(lines) < len(trailerNames)+2 {
		return Record{}, fmt.Errorf("invalid revision message")
	}
	trailer := lines[len(lines)-len(trailerNames)-1:]
	values := make([]string, len(trailerNames))
	for i, name := range trailerNames {
		v, ok := strings.CutPrefix(trailer[i], trailerPrefix+name+": ")
		if !ok {
			return Record{}, fmt.Errorf("invalid field order")
		}
		values[i] = v
	}
	if values[0] != "v3" {
		return Record{}, fmt.Errorf("invalid revision message version")
	}
	var log []byte
	last := trailer[len(trailerNames)]
	if v, ok := strings.CutPrefix(last, trailerLogLength); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > len(b) {
			return Record{}, fmt.Errorf("invalid log length")
		}
		log = slices.Clone(b[:n])
	} else if v, ok := strings.CutPrefix(last, trailerLogHex); ok {
		var err error
		if log, err = hex.DecodeString(v); err != nil {
			return Record{}, err
		}
	} else {
		return Record{}, fmt.Errorf("invalid log field")
	}
	return decodeRevisionFields(values[1], values[2], values[3], values[4], values[5], values[6], log)
}

func decodeRevisionFields(source, archive, number, state, timestamp, author string, log []byte) (Record, error) {
	r := Record{Log: log, State: state}
	decode := func(s string) (string, error) { v, e := hex.DecodeString(s); return string(v), e }
	var err error
	if r.Key.Source, err = decode(source); err != nil {
		return r, err
	}
	if r.Key.Archive, err = decode(archive); err != nil {
		return r, err
	}
	r.Key.Number = number
	if r.Time, err = strconv.ParseInt(timestamp, 10, 64); err != nil {
		return r, err
	}
	if r.Author, err = decode(author); err != nil {
		return r, err
	}
	return r, nil
}

// validRevisionIdentity applies the format-independent provenance limits.
func validRevisionIdentity(r Record) error {
	n, err := Number(r.Key.Number)
	if err != nil || len(n)%2 != 0 || slices.Contains(n, uint64(0)) {
		return fmt.Errorf("invalid actual revision")
	}
	if r.Key.Source == "" || len(r.Key.Source) > 256 || r.Time < 0 || r.Time > 253402300799 || len(r.Author) == 0 || len(r.Author) > MaxAuthor {
		return fmt.Errorf("unsupported revision identity or timestamp")
	}
	for _, c := range []byte(r.Key.Source) {
		if c < 33 || c > 126 {
			return fmt.Errorf("invalid source identity")
		}
	}
	return Path(r.Key.Archive)
}
