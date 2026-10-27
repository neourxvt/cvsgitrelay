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
	return o.SyntheticAt(tree, parents, message, SyntheticTime)
}

// SyntheticAt is Synthetic with an explicit timestamp, which format 3 takes
// from the source data instead of using the fixed one.
func (o Objects) SyntheticAt(tree string, parents []string, message string, timestamp int64) string {
	return o.Put("commit", CommitBytes(tree, parents, Identity, timestamp, []byte(message)))
}

// RevisionMessage encodes a revision event message in the policy's format.
func (p Policy) RevisionMessage(r Record) []byte {
	if p.Version() == 3 {
		return revisionMessageV3(p, r)
	}
	return revisionMessageV2(r)
}

func revisionMessageV2(r Record) []byte {
	return []byte(fmt.Sprintf("cvsgitrelay-revision-v2\nsource=%s\narchive=%s\nrevision=%s\nstate=%s\ntimestamp=%d\nauthor=%s\nlog=%s\n", Hex(r.Key.Source), Hex(r.Key.Archive), r.Key.Number, r.State, r.Time, Hex(r.Author), hex.EncodeToString(r.Log)))
}

// Format 3 trailer lines, in this order. Only the first line is always
// present. The source and format come from the pinned policy, and the author
// and time from the Git header, so they are not repeated here.
const (
	trailerRevision = "CVS: "
	trailerAuthor   = "CVS-Author: "
	trailerState    = "CVS-State: "
	trailerLog      = "CVS-Log: "
	// logNoNewline marks a verbatim log that does not end in a newline.
	logNoNewline = "no-newline"
	// hexPrefix marks a trailer value stored as hex because it is not
	// plain text.
	hexPrefix = "hex:"
)

// EmptyLogText replaces a log without visible text, so a commit never lacks
// a subject line.
const EmptyLogText = "(empty CVS log message)\n"

// shownLog returns the message body for a log and the CVS-Log trailer value
// that restores the exact bytes; the value is empty when the body is the log
// itself. A valid UTF-8 log without NUL bytes and with visible text is shown
// verbatim. Anything else is shown as readable substitute text and its exact
// bytes are kept as hex.
func shownLog(log []byte) (shown, value string) {
	switch {
	case !utf8.Valid(log) || bytes.IndexByte(log, 0) >= 0:
		shown = strings.ReplaceAll(strings.ToValidUTF8(string(log), "\uFFFD"), "\x00", "\uFFFD")
	case len(bytes.TrimSpace(log)) == 0:
		shown = EmptyLogText
	case !bytes.HasSuffix(log, []byte("\n")):
		return string(log), logNoNewline
	default:
		return string(log), ""
	}
	return shown, hexPrefix + hex.EncodeToString(log)
}

// plainField reports whether a value can stand on a trailer line unchanged.
func plainField(s string) bool {
	if s == "" || !utf8.ValidString(s) || strings.HasPrefix(s, hexPrefix) || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// textField writes a value as plain text when possible, else as hex.
func textField(s string) string {
	if plainField(s) {
		return s
	}
	return hexPrefix + Hex(s)
}

func parseTextField(v string) (string, error) {
	if h, ok := strings.CutPrefix(v, hexPrefix); ok {
		b, err := hex.DecodeString(h)
		return string(b), err
	}
	return v, nil
}

// revisionMessageV3 puts the CVS log first, so Git shows it as the subject and
// body, then a blank line and one trailer line naming the archive and CVS
// revision. Further trailer lines appear only for a mapped author, a dead
// revision, or a log that cannot be shown verbatim.
func revisionMessageV3(p Policy, r Record) []byte {
	shown, logValue := shownLog(r.Log)
	var b bytes.Buffer
	b.WriteString(shown)
	if !strings.HasSuffix(shown, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "%s%s %s\n", trailerRevision, r.Key.Number, textField(r.Key.Archive))
	if _, mapped := p.Authors[r.Author]; mapped {
		fmt.Fprintf(&b, "%s%s\n", trailerAuthor, textField(r.Author))
	}
	if r.State == "dead" {
		fmt.Fprintf(&b, "%sdead\n", trailerState)
	}
	if logValue != "" {
		fmt.Fprintf(&b, "%s%s\n", trailerLog, logValue)
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
// hex/number spellings, and unrecognized versions or message boundaries. A
// format 3 message does not carry the author and time, so it needs ParseEvent.
func (p Policy) ParseRevisionMessage(b []byte) (Record, error) {
	if p.Version() == 3 {
		return Record{}, fmt.Errorf("format 3 messages need the commit author line")
	}
	r, err := parseRevisionFieldsV2(b)
	if err != nil {
		return r, err
	}
	return r, p.checkRevision(r, b)
}

// ParseEvent recovers a revision from an event commit's author identity, its
// commit timestamp and its message, and requires all three to be exactly what
// the policy would have written for that revision.
func (p Policy) ParseEvent(identity string, timestamp int64, message []byte) (Record, error) {
	var r Record
	var err error
	if p.Version() == 3 {
		r, err = p.parseEventV3(identity, timestamp, message)
	} else {
		r, err = parseRevisionFieldsV2(message)
	}
	if err != nil {
		return r, err
	}
	if r.Time != timestamp || p.EventAuthor(r.Author) != identity {
		return r, fmt.Errorf("event header differs from its provenance")
	}
	return r, p.checkRevision(r, message)
}

func (p Policy) checkRevision(r Record, message []byte) error {
	if err := validRevisionIdentity(r); err != nil {
		return err
	}
	if !bytes.Equal(p.RevisionMessage(r), message) || (r.State != "live" && r.State != "dead") {
		return fmt.Errorf("noncanonical revision message")
	}
	return nil
}

// AuthorLine splits an event commit's "author NAME <EMAIL> TIME +0000" line.
func AuthorLine(line string) (identity string, timestamp int64, err error) {
	rest, ok := strings.CutPrefix(line, "author ")
	rest, ok2 := strings.CutSuffix(rest, " +0000")
	space := strings.LastIndexByte(rest, ' ')
	if !ok || !ok2 || space < 0 {
		return "", 0, fmt.Errorf("invalid author line")
	}
	timestamp, err = strconv.ParseInt(rest[space+1:], 10, 64)
	if err != nil || strconv.FormatInt(timestamp, 10) != rest[space+1:] {
		return "", 0, fmt.Errorf("invalid author time")
	}
	return rest[:space], timestamp, nil
}

// decodeIdentity recovers the CVS username from an unmapped Git identity.
func decodeIdentity(identity string) (string, error) {
	if name, rest, ok := strings.Cut(identity, " <"); ok && rest == name+"@cvs.invalid>" {
		return name, nil
	}
	if h, ok := strings.CutPrefix(identity, "CVS-"); ok {
		if h, ok = strings.CutSuffix(h, " <cvs@invalid>"); ok {
			b, err := hex.DecodeString(h)
			return string(b), err
		}
	}
	return "", fmt.Errorf("author is neither mapped nor a plain username")
}

// The trailer is the text after the last blank line, and the log is everything
// before it, whatever that says. A message that is not exactly what
// revisionMessageV3 writes for the recovered revision is rejected afterwards.
func (p Policy) parseEventV3(identity string, timestamp int64, b []byte) (Record, error) {
	i := bytes.LastIndex(b, []byte("\n\n"))
	if i < 0 || !bytes.HasSuffix(b, []byte("\n")) {
		return Record{}, fmt.Errorf("invalid revision message")
	}
	body, trailer := b[:i+1], strings.Split(strings.TrimSuffix(string(b[i+2:]), "\n"), "\n")
	number, archive, ok := strings.Cut(strings.TrimPrefix(trailer[0], trailerRevision), " ")
	if !strings.HasPrefix(trailer[0], trailerRevision) || !ok {
		return Record{}, fmt.Errorf("invalid revision trailer")
	}
	r := Record{Key: Key{Source: p.Source, Number: number}, Time: timestamp, State: "live", Log: slices.Clone(body)}
	var err error
	if r.Key.Archive, err = parseTextField(archive); err != nil {
		return r, err
	}
	// Optional lines follow the first one, each at most once and in order.
	// Without an author line the Git author is a plain or hex username.
	rest := trailer[1:]
	if len(rest) > 0 && strings.HasPrefix(rest[0], trailerAuthor) {
		if r.Author, err = parseTextField(strings.TrimPrefix(rest[0], trailerAuthor)); err != nil {
			return r, err
		}
		rest = rest[1:]
	} else if r.Author, err = decodeIdentity(identity); err != nil {
		return r, err
	}
	if len(rest) > 0 && strings.HasPrefix(rest[0], trailerState) {
		r.State, rest = strings.TrimPrefix(rest[0], trailerState), rest[1:]
	}
	if len(rest) > 0 && strings.HasPrefix(rest[0], trailerLog) {
		value := strings.TrimPrefix(rest[0], trailerLog)
		rest = rest[1:]
		if value == logNoNewline {
			r.Log = slices.Clone(body[:len(body)-1])
		} else if h, ok := strings.CutPrefix(value, hexPrefix); ok {
			if r.Log, err = hex.DecodeString(h); err != nil {
				return r, err
			}
		} else {
			return r, fmt.Errorf("invalid log trailer")
		}
	}
	if len(rest) != 0 {
		return r, fmt.Errorf("unrecognized trailer lines")
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
