package canonical

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
func RevisionMessage(r Record) []byte {
	return []byte(fmt.Sprintf("cvsgitrelay-revision-v2\nsource=%s\narchive=%s\nrevision=%s\nstate=%s\ntimestamp=%d\nauthor=%s\nlog=%s\n", Hex(r.Key.Source), Hex(r.Key.Archive), r.Key.Number, r.State, r.Time, Hex(r.Author), hex.EncodeToString(r.Log)))
}
func (o Objects) Event(r Record, tree, parent string) string {
	return o.Put("commit", CommitBytes(tree, []string{parent}, "CVS-"+Hex(r.Author)+" <cvs@invalid>", r.Time, RevisionMessage(r)))
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
func ParseRevisionMessage(b []byte) (Record, error) {
	var r Record
	f := strings.Split(string(b), "\n")
	if len(f) != 9 || f[0] != "cvsgitrelay-revision-v2" || f[8] != "" {
		return r, fmt.Errorf("invalid revision message")
	}
	names := []string{"source", "archive", "revision", "state", "timestamp", "author", "log"}
	values := map[string]string{}
	for i, n := range names {
		v, ok := strings.CutPrefix(f[i+1], n+"=")
		if !ok {
			return r, fmt.Errorf("invalid field order")
		}
		values[n] = v
	}
	decode := func(s string) (string, error) { v, e := hex.DecodeString(s); return string(v), e }
	var err error
	if r.Key.Source, err = decode(values["source"]); err != nil {
		return r, err
	}
	if r.Key.Archive, err = decode(values["archive"]); err != nil {
		return r, err
	}
	r.Key.Number = values["revision"]
	n, err := Number(r.Key.Number)
	if err != nil || len(n)%2 != 0 || slices.Contains(n, uint64(0)) {
		return r, fmt.Errorf("invalid actual revision")
	}
	r.State = values["state"]
	if r.Time, err = strconv.ParseInt(values["timestamp"], 10, 64); err != nil {
		return r, err
	}
	if r.Author, err = decode(values["author"]); err != nil {
		return r, err
	}
	if r.Log, err = hex.DecodeString(values["log"]); err != nil {
		return r, err
	}
	if r.Key.Source == "" || len(r.Key.Source) > 256 || r.Time < 0 || r.Time > 253402300799 || len(r.Author) == 0 || len(r.Author) > MaxAuthor {
		return r, fmt.Errorf("unsupported revision identity or timestamp")
	}
	for _, c := range []byte(r.Key.Source) {
		if c < 33 || c > 126 {
			return r, fmt.Errorf("invalid source identity")
		}
	}
	if err := Path(r.Key.Archive); err != nil {
		return r, err
	}
	if !bytes.Equal(RevisionMessage(r), b) || (r.State != "live" && r.State != "dead") {
		return r, fmt.Errorf("noncanonical revision message")
	}
	return r, nil
}
