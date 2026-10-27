// Package canonical defines the v2 SHA-1 format independently of CVS transport,
// Git plumbing, publication history, and local caches.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"
)

const SyntheticTime int64 = 946684800
const Identity = "CVSGITRELAY <relay@invalid>"
const MaxPath = 4096
const MaxAuthor = 256
const MaxRef = 4096

type Mapping struct{ Archive, Checkout string }

// Policy deliberately contains no locator, destination, or runtime setting.
// Its fixed profiles specify normalization, aliases, author identities and modes.
type Policy struct {
	Source     string
	Module     string
	Submodules string
	Verify     string
	Mappings   []Mapping
	// Format selects the revision event encoding. Zero is format 2, which
	// predates this field, so existing configurations keep their encoding.
	Format int `json:",omitempty"`
	// Authors maps raw CVS usernames to "Name <email>" Git identities. Only
	// format 3 supports it; like the rest of the policy it is fixed at init.
	Authors map[string]string `json:",omitempty"`
}

// Version returns the destination format number this policy encodes.
func (p Policy) Version() int {
	if p.Format == 0 {
		return 2
	}
	return p.Format
}

func U(n uint64) []byte     { return binary.BigEndian.AppendUint64(nil, n) }
func B(b []byte) []byte     { return append(U(uint64(len(b))), b...) }
func frame(s string) []byte { return B([]byte(s)) }
func Hex(s string) string   { return hex.EncodeToString([]byte(s)) }
func Hash(domain string, b []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(append([]byte(domain+"\x00"), b...)))
}

// ASCII source IDs avoid Unicode normalization aliases. UTF-8 archive and author
// bytes remain opaque; IDs are operator-assigned, never generated.
func (p Policy) Validate() error {
	if len(p.Source) == 0 || len(p.Source) > 256 {
		return fmt.Errorf("source_id must contain 1..256 printable ASCII bytes")
	}
	for _, c := range []byte(p.Source) {
		if c < 33 || c > 126 {
			return fmt.Errorf("source_id must use printable ASCII without spaces")
		}
	}
	if err := Path(p.Module); err != nil {
		return err
	}
	if p.Submodules != "include" && p.Submodules != "exclude" {
		return fmt.Errorf("invalid submodule policy")
	}
	if p.Verify != "full" && p.Verify != "trust-append-only" {
		return fmt.Errorf("invalid verification policy")
	}
	if p.Format != 0 && p.Format != 3 {
		return fmt.Errorf("unsupported destination format")
	}
	if len(p.Authors) > 0 && p.Version() != 3 {
		return fmt.Errorf("author maps require destination format 3")
	}
	for name, identity := range p.Authors {
		if len(name) == 0 || len(name) > MaxAuthor {
			return fmt.Errorf("author map usernames must contain 1..%d bytes", MaxAuthor)
		}
		if !ValidIdentity(identity) {
			return fmt.Errorf("author map identity must have the form \"Name <email>\"")
		}
	}
	if len(p.Mappings) == 0 {
		return fmt.Errorf("explicit archive mapping required")
	}
	for i, m := range p.Mappings {
		if err := Path(m.Archive); err != nil {
			return err
		}
		if m.Checkout != "" {
			if err := Path(m.Checkout); err != nil {
				return err
			}
		}
		for j := 0; j < i; j++ {
			o := p.Mappings[j]
			if o.Archive >= m.Archive || strings.HasPrefix(m.Archive, o.Archive+"/") || o.Checkout == m.Checkout {
				return fmt.Errorf("ambiguous or unsorted scope mapping")
			}
		}
	}
	return nil
}

// Format 2 bytes are unchanged from before format 3 existed. Format 3 names
// its author profile and version, and appends the sorted author map.
func (p Policy) Bytes() []byte {
	author, version := "hex-author-v2", "2"
	if p.Version() == 3 {
		author, version = "mapped-author-compact-v3", "3"
	}
	var b []byte
	for _, s := range []string{p.Source, p.Module, p.Submodules, "gnu-cvs-rlog-v2", "attic-strip-v2", "identical-membership-alias-v2", author, "100644", "kb", "cvs-exp-dead-absent-v2", version, "sha1", p.Verify} {
		b = append(b, frame(s)...)
	}
	b = append(b, U(uint64(len(p.Mappings)))...)
	for _, m := range p.Mappings {
		b = append(b, frame(m.Archive)...)
		b = append(b, frame(m.Checkout)...)
	}
	if p.Version() == 3 {
		names := SortedKeys(p.Authors)
		b = append(b, U(uint64(len(names)))...)
		for _, name := range names {
			b = append(b, frame(name)...)
			b = append(b, frame(p.Authors[name])...)
		}
	}
	return b
}
func (p Policy) Digest() string { return Hash("cvsgitrelay-policy-v2", p.Bytes()) }

func Number(s string) ([]uint64, error) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid revision number")
	}
	n := make([]uint64, len(parts))
	for i, p := range parts {
		v, e := strconv.ParseUint(p, 10, 64)
		if e != nil || strconv.FormatUint(v, 10) != p {
			return nil, fmt.Errorf("noncanonical revision number")
		}
		n[i] = v
	}
	return n, nil
}
func CompareNumber(a, b string) int {
	x, _ := Number(a)
	y, _ := Number(b)
	return slices.Compare(x, y)
}
func Line(number string) string {
	if strings.Count(number, ".") == 1 {
		return "trunk"
	}
	return number[:strings.LastIndexByte(number, '.')]
}
func Fork(line string) string { return line[:strings.LastIndexByte(line, '.')] }

// Branch normalizes CVS's magic .0. branch symbols without changing real keys.
func Branch(s string) (string, bool) {
	n, e := Number(s)
	if e != nil {
		return "", false
	}
	if len(n) >= 4 && n[len(n)-2] == 0 {
		n = append(slices.Clone(n[:len(n)-2]), n[len(n)-1])
	}
	if len(n)%2 != 1 {
		return "", false
	}
	parts := make([]string, len(n))
	for i, v := range n {
		if v == 0 {
			return "", false
		}
		parts[i] = strconv.FormatUint(v, 10)
	}
	return strings.Join(parts, "."), true
}

func Path(p string) error {
	if p == "" || len(p) > MaxPath || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\r\n\t") {
		return fmt.Errorf("unsafe or unsupported path")
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." || strings.EqualFold(s, ".git") || s == "CVS" {
			return fmt.Errorf("unsafe path component")
		}
	}
	return nil
}

type Key struct{ Source, Archive, Number string }

func (k Key) Bytes() []byte {
	b := append(frame(k.Source), frame(k.Archive)...)
	n, _ := Number(k.Number)
	b = append(b, U(uint64(len(n)))...)
	for _, v := range n {
		b = append(b, U(v)...)
	}
	return b
}
func (k Key) ID() string { return string(k.Bytes()) }

type Record struct {
	Key     Key
	Path    string
	Time    int64
	Author  string
	Log     []byte
	State   string
	Parent  *Key
	Present bool
	Content []byte
}

func (r Record) Bytes() []byte { return append(r.prefix(), r.Content...) }

// prefix is the Record.Bytes encoding up to and including the content length.
// The content always comes last, so hashes can stream it without a copy.
func (r Record) prefix() []byte {
	b := r.Key.Bytes()
	b = append(b, U(uint64(r.Time))...)
	for _, v := range []string{r.Author, string(r.Log), r.State} {
		b = append(b, frame(v)...)
	}
	// The ancestry record is a counted list of zero or one full key. Zero is
	// the distinguished absent-parent value; this is framed as one field.
	a := U(0)
	if r.Parent != nil {
		a = append(U(1), r.Parent.Bytes()...)
	}
	b = append(b, B(a)...)
	presence := "absent"
	if r.Present {
		presence = "present"
	}
	b = append(b, frame(presence)...)
	return append(b, U(uint64(len(r.Content)))...)
}

// Fingerprint equals Hash("cvsgitrelay-revision-record-v2", r.Bytes()).
func (r Record) Fingerprint() string {
	h := hashWriter("cvsgitrelay-revision-record-v2")
	h.Write(r.prefix())
	h.Write(r.Content)
	return hex.EncodeToString(h.Sum(nil))
}

// hashWriter starts the same domain-separated SHA-256 stream as Hash.
func hashWriter(domain string) hash.Hash {
	h := sha256.New()
	h.Write([]byte(domain + "\x00"))
	return h
}

type Symbol struct {
	Archive, Name, Target string
	Branch                bool
}

func (s Symbol) Bytes(source string) []byte {
	b := append(frame(s.Archive), frame(s.Name)...)
	if s.Branch {
		b = append(b, frame("branch")...)
		return append(b, frame(s.Target)...)
	}
	b = append(b, frame("tag")...)
	return append(b, B((Key{source, s.Archive, s.Target}).Bytes())...)
}

type Inventory struct {
	Records []Record
	Symbols []Symbol
}

// Normalize validates byte identities before any objects can be published.
func (in Inventory) Normalize(p Policy) (Inventory, error) {
	if err := p.Validate(); err != nil {
		return Inventory{}, err
	}
	in.Records = slices.Clone(in.Records)
	in.Symbols = slices.Clone(in.Symbols)
	slices.SortFunc(in.Records, func(a, b Record) int { return bytes.Compare(a.Key.Bytes(), b.Key.Bytes()) })
	paths := map[string]string{}
	archives := map[string]string{}
	keys := map[string]Record{}
	for _, r := range in.Records {
		n, e := Number(r.Key.Number)
		if e != nil || len(n)%2 != 0 || slices.Contains(n, uint64(0)) {
			return Inventory{}, fmt.Errorf("invalid actual revision")
		}
		if r.Key.Source != p.Source {
			return Inventory{}, fmt.Errorf("source identity mismatch")
		}
		if err := Path(r.Key.Archive); err != nil {
			return Inventory{}, err
		}
		if err := Path(r.Path); err != nil {
			return Inventory{}, err
		}
		mapped := ""
		for _, m := range p.Mappings {
			if p.Submodules == "exclude" && m.Checkout != "" {
				continue
			}
			if strings.HasPrefix(r.Key.Archive, m.Archive+"/") {
				if mapped != "" {
					return Inventory{}, fmt.Errorf("ambiguous mapping")
				}
				mapped = strings.TrimPrefix(r.Key.Archive, m.Archive+"/")
				if m.Checkout != "" {
					mapped = m.Checkout + "/" + mapped
				}
			}
		}
		if mapped != r.Path {
			return Inventory{}, fmt.Errorf("revision outside policy mapping")
		}
		if r.Time < 0 || r.Time > 253402300799 || len(r.Author) == 0 || len(r.Author) > MaxAuthor {
			return Inventory{}, fmt.Errorf("unsupported timestamp or author length")
		}
		if (r.State != "live" && r.State != "dead") || (!r.Present && (r.State == "live" || len(r.Content) != 0)) {
			return Inventory{}, fmt.Errorf("invalid state/content presence")
		}
		if _, ok := keys[r.Key.ID()]; ok {
			return Inventory{}, fmt.Errorf("duplicate revision key")
		}
		keys[r.Key.ID()] = r
		if old := paths[r.Path]; old != "" && old != r.Key.Archive {
			return Inventory{}, fmt.Errorf("checkout path collision")
		}
		paths[r.Path] = r.Key.Archive
		if old := archives[r.Key.Archive]; old != "" && old != r.Path {
			return Inventory{}, fmt.Errorf("archive remapping")
		}
		archives[r.Key.Archive] = r.Path
	}
	for path := range paths {
		for parent := path; strings.Contains(parent, "/"); {
			parent = parent[:strings.LastIndexByte(parent, '/')]
			if paths[parent] != "" {
				return Inventory{}, fmt.Errorf("file/directory collision")
			}
		}
	}
	for _, r := range in.Records {
		if r.Parent != nil {
			parent, ok := keys[r.Parent.ID()]
			if !ok {
				return Inventory{}, fmt.Errorf("inaccessible predecessor")
			}
			if parent.Key.Archive != r.Key.Archive || parent.Key.Source != p.Source || CompareNumber(parent.Key.Number, r.Key.Number) >= 0 {
				return Inventory{}, fmt.Errorf("invalid ancestry")
			}
			if Line(parent.Key.Number) != Line(r.Key.Number) && (Line(r.Key.Number) == "trunk" || parent.Key.Number != Fork(Line(r.Key.Number))) {
				return Inventory{}, fmt.Errorf("invalid forkpoint")
			}
		} else if Line(r.Key.Number) != "trunk" {
			return Inventory{}, fmt.Errorf("branch lacks forkpoint")
		}
	}
	// A symbol's archive identity and target are part of its framing. Equal
	// names on different archives remain distinct observations.
	slices.SortFunc(in.Symbols, func(a, b Symbol) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Archive, b.Archive)
	})
	for i, s := range in.Symbols {
		if s.Name == "" || len(s.Name) > 1024 || strings.ContainsAny(s.Name, "\x00\r\n") {
			return Inventory{}, fmt.Errorf("unsupported symbol")
		}
		if i > 0 && in.Symbols[i-1].Name == s.Name {
			prev := in.Symbols[i-1]
			if prev.Archive == s.Archive || prev.Branch != s.Branch {
				return Inventory{}, fmt.Errorf("duplicate or conflicting symbol")
			}
		}
		target := s.Target
		if s.Branch {
			b, ok := Branch(target)
			if !ok || b != target {
				return Inventory{}, fmt.Errorf("noncanonical branch symbol")
			}
			target = Fork(target)
		}
		if _, ok := keys[(Key{p.Source, s.Archive, target}).ID()]; !ok {
			return Inventory{}, fmt.Errorf("symbol target unavailable")
		}
	}
	return in, nil
}

// Digest streams the framed records and symbols into the hash instead of
// assembling one buffer holding every revision body.
func (in Inventory) Digest(source string) string {
	h := hashWriter("cvsgitrelay-source-v2")
	h.Write(U(uint64(len(in.Records))))
	for _, r := range in.Records {
		h.Write(B(r.Key.Bytes()))
		prefix := r.prefix()
		h.Write(U(uint64(len(prefix) + len(r.Content))))
		h.Write(prefix)
		h.Write(r.Content)
	}
	h.Write(U(uint64(len(in.Symbols))))
	for _, s := range in.Symbols {
		h.Write(B(s.Bytes(source)))
	}
	return hex.EncodeToString(h.Sum(nil))
}
