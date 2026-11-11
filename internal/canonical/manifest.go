package canonical

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type RevisionEntry struct{ Fingerprint, Event string }
type Manifest struct {
	Source, PolicyDigest, SourceDigest, Verify string
	Revisions                                  map[string]RevisionEntry // raw RevisionKeyBytes
	Lines                                      map[string]string
	Refs                                       map[string]string
	// Growing is set for manifests of growing history, which record what
	// was published so that later syncs can only add to it.
	Growing bool
	// Removed holds the IDs of published revisions that the source no longer
	// lists. Their entries stay in Revisions, as their commits stay in the
	// history.
	Removed map[string]bool
	// Members maps every published file line (fileLineID) to its line ID.
	// A file line never moves to another line.
	Members map[string]string
	// Bindings maps every published branch ref to the line it follows. A ref
	// whose branch disappeared from CVS keeps its binding and its last value.
	Bindings map[string]string
	// Tags maps every published CVS tag name to its versions.
	Tags map[string]TagEntry
}

// TagEntry records the published versions of one CVS tag. Version 1 is
// published under the tag's own ref, and every later one, which CVS made by
// moving the tag, under that ref plus "@<version>". Selection is the digest
// of the revisions the latest version selects.
type TagEntry struct {
	Versions  int
	Selection string
}

// growingManifest returns an empty manifest of growing history.
func growingManifest(m Manifest) Manifest {
	m.Growing = true
	m.Removed, m.Members, m.Bindings, m.Tags = map[string]bool{}, map[string]string{}, map[string]string{}, map[string]TagEntry{}
	return m
}

func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
func (m Manifest) Bytes() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "cvsgitrelay-manifest-v2\nformat=2\ngit-object-format=sha1\nsource=%s\npolicy-sha256=%s\nsource-sha256=%s\nverify=%s\n", Hex(m.Source), m.PolicyDigest, m.SourceDigest, m.Verify)
	if m.Growing {
		b.WriteString("history=growing\n")
	}
	fmt.Fprintf(&b, "revision-count=%d\n", len(m.Revisions))
	for _, k := range SortedKeys(m.Revisions) {
		v := m.Revisions[k]
		fmt.Fprintf(&b, "R %s %s %s\n", Hex(k), v.Fingerprint, v.Event)
	}
	fmt.Fprintf(&b, "line-count=%d\n", len(m.Lines))
	for _, k := range SortedKeys(m.Lines) {
		fmt.Fprintf(&b, "L %s %s\n", Hex(k), m.Lines[k])
	}
	fmt.Fprintf(&b, "ref-count=%d\n", len(m.Refs))
	for _, k := range SortedKeys(m.Refs) {
		fmt.Fprintf(&b, "P %s %s\n", Hex(k), m.Refs[k])
	}
	if !m.Growing {
		return []byte(b.String())
	}
	fmt.Fprintf(&b, "removed-count=%d\n", len(m.Removed))
	for _, k := range SortedKeys(m.Removed) {
		fmt.Fprintf(&b, "X %s\n", Hex(k))
	}
	fmt.Fprintf(&b, "member-count=%d\n", len(m.Members))
	for _, k := range SortedKeys(m.Members) {
		fmt.Fprintf(&b, "M %s %s\n", Hex(k), Hex(m.Members[k]))
	}
	fmt.Fprintf(&b, "binding-count=%d\n", len(m.Bindings))
	for _, k := range SortedKeys(m.Bindings) {
		fmt.Fprintf(&b, "B %s %s\n", Hex(k), Hex(m.Bindings[k]))
	}
	fmt.Fprintf(&b, "tag-count=%d\n", len(m.Tags))
	for _, k := range SortedKeys(m.Tags) {
		fmt.Fprintf(&b, "T %s %d %s\n", Hex(k), m.Tags[k].Versions, m.Tags[k].Selection)
	}
	return []byte(b.String())
}

func DecodeKey(b []byte) (Key, error) {
	var k Key
	read := func() ([]byte, error) {
		if len(b) < 8 {
			return nil, fmt.Errorf("truncated key")
		}
		n := binary.BigEndian.Uint64(b)
		b = b[8:]
		if n > uint64(len(b)) {
			return nil, fmt.Errorf("invalid key length")
		}
		v := b[:int(n)]
		b = b[int(n):]
		return v, nil
	}
	s, e := read()
	if e != nil {
		return k, e
	}
	k.Source = string(s)
	s, e = read()
	if e != nil {
		return k, e
	}
	k.Archive = string(s)
	if len(b) < 8 {
		return k, fmt.Errorf("missing numeric key")
	}
	n := binary.BigEndian.Uint64(b)
	b = b[8:]
	if n < 2 || n%2 != 0 || n > uint64(len(b)/8) || n*8 != uint64(len(b)) {
		return k, fmt.Errorf("invalid numeric key")
	}
	parts := make([]string, int(n))
	for i := range parts {
		v := binary.BigEndian.Uint64(b[i*8:])
		if v == 0 {
			return k, fmt.Errorf("invalid actual revision component")
		}
		parts[i] = strconv.FormatUint(v, 10)
	}
	k.Number = strings.Join(parts, ".")
	return k, nil
}

// ParseManifest accepts only the exact serializer's language, including order,
// mandatory counts, uniqueness and lowercase byte representations.
func ParseManifest(b []byte) (Manifest, error) { return parseManifest(b, ProjectionRef, false) }

// parseManifest parses with the given projection ref predicate. A manifest of
// growing history has its own header line and sections, and no other has.
func parseManifest(b []byte, projection func(string) bool, growing bool) (Manifest, error) {
	m := Manifest{Revisions: map[string]RevisionEntry{}, Lines: map[string]string{}, Refs: map[string]string{}}
	if growing {
		m = growingManifest(m)
	}
	lines := strings.Split(string(b), "\n")
	pos := 0
	take := func(prefix string) (string, error) {
		if pos >= len(lines) || !strings.HasPrefix(lines[pos], prefix) {
			return "", fmt.Errorf("invalid manifest field")
		}
		s := strings.TrimPrefix(lines[pos], prefix)
		pos++
		return s, nil
	}
	for _, fixed := range []string{"cvsgitrelay-manifest-v2", "format=2", "git-object-format=sha1"} {
		s, e := take(fixed)
		if e != nil || s != "" {
			return m, fmt.Errorf("invalid manifest format")
		}
	}
	decode := func(s string) (string, error) {
		b, e := hex.DecodeString(s)
		if e != nil || Hex(string(b)) != s {
			return "", fmt.Errorf("invalid canonical hex")
		}
		return string(b), nil
	}
	s, e := take("source=")
	if e != nil {
		return m, e
	}
	m.Source, e = decode(s)
	if e != nil {
		return m, e
	}
	if m.PolicyDigest, e = take("policy-sha256="); e != nil {
		return m, e
	}
	if m.SourceDigest, e = take("source-sha256="); e != nil {
		return m, e
	}
	for _, d := range []string{m.PolicyDigest, m.SourceDigest} {
		raw, e := decode(d)
		if e != nil || len(raw) != 32 {
			return m, fmt.Errorf("invalid digest")
		}
	}
	if m.Verify, e = take("verify="); e != nil {
		return m, e
	}
	if m.Verify != "full" && m.Verify != "trust-append-only" {
		return m, fmt.Errorf("invalid verification policy")
	}
	if growing {
		if s, e := take("history=growing"); e != nil || s != "" {
			return m, fmt.Errorf("invalid manifest history")
		}
	}
	for _, section := range []struct {
		label, row string
		fields     int
	}{{"revision", "R", 4}, {"line", "L", 3}, {"ref", "P", 3}} {
		count, e := take(section.label + "-count=")
		if e != nil {
			return m, e
		}
		n, e := strconv.ParseUint(count, 10, 64)
		if e != nil || n > uint64(len(lines)-pos) {
			return m, fmt.Errorf("invalid count")
		}
		for i := uint64(0); i < n; i++ {
			f := strings.Split(lines[pos], " ")
			pos++
			if len(f) != section.fields || f[0] != section.row {
				return m, fmt.Errorf("invalid manifest row")
			}
			k, e := decode(f[1])
			if e != nil {
				return m, e
			}
			oid := f[len(f)-1]
			if !ValidOID(oid) {
				return m, fmt.Errorf("invalid oid")
			}
			switch section.row {
			case "R":
				key, e := DecodeKey([]byte(k))
				if e != nil || key.Source != m.Source {
					return m, fmt.Errorf("invalid revision key")
				}
				finger, e := decode(f[2])
				if e != nil || len(finger) != 32 {
					return m, fmt.Errorf("invalid fingerprint")
				}
				if _, ok := m.Revisions[k]; ok {
					return m, fmt.Errorf("duplicate key")
				}
				m.Revisions[k] = RevisionEntry{f[2], oid}
			case "L":
				if k == "" {
					return m, fmt.Errorf("empty logical line ID")
				}
				if _, ok := m.Lines[k]; ok {
					return m, fmt.Errorf("duplicate line")
				}
				m.Lines[k] = oid
			case "P":
				if !projection(k) {
					return m, fmt.Errorf("invalid projection ref")
				}
				if _, ok := m.Refs[k]; ok {
					return m, fmt.Errorf("duplicate ref")
				}
				m.Refs[k] = oid
			}
		}
	}
	if growing {
		if e := parseGrowingSections(&m, lines, &pos, decode); e != nil {
			return m, e
		}
	}
	if pos != len(lines)-1 || lines[pos] != "" || !bytes.Equal(m.Bytes(), b) {
		return m, fmt.Errorf("noncanonical manifest")
	}
	return m, nil
}

// parseGrowingSections reads the sections that only growing history has and
// checks that they refer to published revisions, lines and refs. Ordering,
// uniqueness and spelling are left to the final comparison with Bytes.
func parseGrowingSections(m *Manifest, lines []string, pos *int, decode func(string) (string, error)) error {
	for _, section := range []struct {
		label, row string
		fields     int
	}{{"removed", "X", 2}, {"member", "M", 3}, {"binding", "B", 3}, {"tag", "T", 4}} {
		if *pos >= len(lines) {
			return fmt.Errorf("truncated manifest")
		}
		count, ok := strings.CutPrefix(lines[*pos], section.label+"-count=")
		if !ok {
			return fmt.Errorf("invalid manifest field")
		}
		*pos++
		n, e := strconv.ParseUint(count, 10, 64)
		if e != nil || n > uint64(len(lines)-*pos) {
			return fmt.Errorf("invalid count")
		}
		for i := uint64(0); i < n; i++ {
			f := strings.Split(lines[*pos], " ")
			*pos++
			if len(f) != section.fields || f[0] != section.row {
				return fmt.Errorf("invalid manifest row")
			}
			k, e := decode(f[1])
			if e != nil || k == "" {
				return fmt.Errorf("invalid manifest key")
			}
			var value string
			if section.row == "M" || section.row == "B" {
				if value, e = decode(f[2]); e != nil || m.Lines[value] == "" {
					return fmt.Errorf("manifest row names an unknown line")
				}
			}
			switch section.row {
			case "X":
				if _, ok := m.Revisions[k]; !ok {
					return fmt.Errorf("removed revision was never published")
				}
				m.Removed[k] = true
			case "M":
				m.Members[k] = value
			case "B":
				if m.Refs[k] == "" {
					return fmt.Errorf("binding of an unpublished ref")
				}
				m.Bindings[k] = value
			case "T":
				versions, e := strconv.Atoi(f[2])
				selection, e2 := decode(f[3])
				if e != nil || e2 != nil || versions < 1 || len(selection) != 32 {
					return fmt.Errorf("invalid tag row")
				}
				m.Tags[k] = TagEntry{versions, f[3]}
			}
		}
	}
	return nil
}

func EncodedRef(prefix, name string) (string, error) {
	ref := prefix + "b" + Hex(name)
	if len(ref) > MaxRef {
		return "", fmt.Errorf("unsupported ref length")
	}
	return ref, nil
}
func ProjectionRef(ref string) bool {
	if ref == "refs/heads/cvs/trunk" {
		return true
	}
	for _, p := range []string{"refs/heads/cvs/b", "refs/tags/cvs/b", "refs/cvsgitrelay/lines/b"} {
		if strings.HasPrefix(ref, p) {
			s := strings.TrimPrefix(ref, p)
			b, e := hex.DecodeString(s)
			return s != "" && e == nil && Hex(string(b)) == s && len(ref) <= MaxRef
		}
	}
	return false
}
