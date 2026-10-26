package relay

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"cvsgitrelay/internal/diag"
)

type Revision struct {
	Path        string
	Number      string
	Author      string
	State       string
	Time        int64
	Log         []byte
	CommitID    string
	Line        string
	Predecessor string
	Ref         string
}

type File struct {
	Path          string
	Head          string
	DefaultBranch string
	Symbols       map[string]string
	Revisions     []*Revision
}

type Inventory struct {
	Files     map[string]*File
	Revisions map[string]*Revision
	Ordered   []*Revision
	Branches  map[string]map[string]string // destination -> path -> forkpoint
	Trunk     int
	Dead      int
	Vendor    int
}

func key(path, number string) string { return path + "\x00" + number }
func hexBytes(s string) string       { return hex.EncodeToString([]byte(s)) }
func digest(data []byte) string      { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func numbers(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strconv.Itoa(n) != part {
			return nil, fmt.Errorf("invalid dotted revision %q", s)
		}
		out = append(out, n)
	}
	if len(out) < 2 {
		return nil, fmt.Errorf("incomplete revision %q", s)
	}
	return out, nil
}

func compareNumber(a, b string) int {
	x, _ := numbers(a)
	y, _ := numbers(b)
	return slices.Compare(x, y)
}

func branchNumber(s string) (string, bool) {
	n, err := numbers(s)
	if err != nil {
		return "", false
	}
	if len(n) >= 4 && n[len(n)-2] == 0 {
		n = append(n[:len(n)-2], n[len(n)-1])
	}
	if len(n)%2 != 1 {
		return "", false
	}
	parts := make([]string, len(n))
	for i, v := range n {
		if v == 0 {
			return "", false
		}
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, "."), true
}

// CVS's text frontend cannot represent control characters in path headers.
// Git receives all other supported path bytes through NUL-delimited plumbing.
func validatePath(path string) error {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r\n\t") {
		return diag.New("source", "path.validate", "unsupported or ambiguous path", nil, "path_hex", hexBytes(path))
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") || part == "CVS" {
			return diag.New("source", "path.validate", "unsafe CVS/Git path component", nil, "path_hex", hexBytes(path))
		}
	}
	return nil
}

func buildInventory(files []*File) (*Inventory, error) {
	in := &Inventory{Files: map[string]*File{}, Revisions: map[string]*Revision{}, Branches: map[string]map[string]string{}}
	for _, f := range files {
		if _, exists := in.Files[f.Path]; exists {
			return nil, diag.New("source", "inventory", "duplicate logical path (possible Attic collision)", nil, "path_hex", hexBytes(f.Path))
		}
		in.Files[f.Path] = f
		lines := map[string][]*Revision{}
		for _, r := range f.Revisions {
			n, err := numbers(r.Number)
			if err != nil || len(n)%2 != 0 || n[0] != 1 || slices.Contains(n, 0) {
				return nil, diag.New("unsupported", "inventory", "unsupported CVS revision numbering", err, "path_hex", hexBytes(f.Path), "revision", r.Number)
			}
			r.Path = f.Path
			r.Line = r.Number[:strings.LastIndexByte(r.Number, '.')]
			if len(n) == 2 {
				r.Line = "trunk"
				r.Ref = trunkRef
				in.Trunk++
			}
			if r.State == "dead" {
				in.Dead++
			}
			if strings.HasPrefix(r.Number, "1.1.1.") {
				in.Vendor++
			}
			k := key(r.Path, r.Number)
			if in.Revisions[k] != nil {
				return nil, diag.New("source", "inventory", "duplicate revision", nil, "path_hex", hexBytes(r.Path), "revision", r.Number)
			}
			in.Revisions[k] = r
			lines[r.Line] = append(lines[r.Line], r)
		}
		if len(lines["trunk"]) == 0 {
			return nil, diag.New("source", "inventory", "file has no accessible trunk", nil, "path_hex", hexBytes(f.Path))
		}
		for line, revs := range lines {
			slices.SortFunc(revs, func(a, b *Revision) int { return compareNumber(a.Number, b.Number) })
			for i, r := range revs {
				n, _ := numbers(r.Number)
				if n[len(n)-1] != i+1 {
					return nil, diag.New("source", "inventory", "revision gap; administrative removal or incomplete enumeration requires investigation", nil, "path_hex", hexBytes(f.Path), "revision", r.Number, "expected_ordinal", i+1)
				}
				if i > 0 {
					r.Predecessor = revs[i-1].Number
				} else if line != "trunk" {
					r.Predecessor = line[:strings.LastIndexByte(line, '.')]
				}
			}
			if line == "trunk" {
				if f.Head != revs[len(revs)-1].Number {
					return nil, diag.New("source", "inventory", "declared head does not match trunk", nil, "path_hex", hexBytes(f.Path), "head", f.Head)
				}
				continue
			}
			var names []string
			for name, num := range f.Symbols {
				if b, ok := branchNumber(num); ok && b == line {
					names = append(names, name)
				}
			}
			if len(names) > 1 {
				return nil, diag.New("unsupported", "inventory", "multiple symbols own one revision line; branch alias semantics require an explicit policy", nil, "path_hex", hexBytes(f.Path), "branch_names_hex", mapHex(names))
			}
			ref := archiveRef(f.Path, line)
			if len(names) == 1 {
				ref = namedRef(names[0])
			}
			for _, r := range revs {
				r.Ref = ref
			}
		}
		// Symbols with no revisions still define a checkoutable seed tree.
		for name, num := range f.Symbols {
			if b, ok := branchNumber(num); ok {
				ref := namedRef(name)
				if in.Branches[ref] == nil {
					in.Branches[ref] = map[string]string{}
				}
				in.Branches[ref][f.Path] = b[:strings.LastIndexByte(b, '.')]
			}
		}
		for line, revs := range lines {
			if line == "trunk" {
				continue
			}
			ref := revs[0].Ref
			if in.Branches[ref] == nil {
				in.Branches[ref] = map[string]string{}
			}
			in.Branches[ref][f.Path] = revs[0].Predecessor
		}
	}
	for _, r := range in.Revisions {
		if r.Predecessor != "" && in.Revisions[key(r.Path, r.Predecessor)] == nil {
			return nil, diag.New("source", "inventory", "missing forkpoint or predecessor", nil, "path_hex", hexBytes(r.Path), "revision", r.Number, "predecessor", r.Predecessor)
		}
		in.Ordered = append(in.Ordered, r)
	}
	for ref, members := range in.Branches {
		for p, rev := range members {
			if in.Revisions[key(p, rev)] == nil {
				return nil, diag.New("source", "inventory", "branch symbol points to inaccessible forkpoint", nil, "ref", ref, "path_hex", hexBytes(p), "forkpoint", rev)
			}
		}
	}
	// Ancestry is scheduled separately from the timestamp tie breaker.
	slices.SortFunc(in.Ordered, func(a, b *Revision) int {
		if c := cmp.Compare(a.Time, b.Time); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Ref, b.Ref); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return compareNumber(a.Number, b.Number)
	})
	return in, nil
}

func mapHex(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = hexBytes(v)
	}
	slices.Sort(out)
	return out
}

func parseDate(s string) (int64, error) {
	for _, layout := range []string{"2006/01/02 15:04:05", "2006-01-02 15:04:05 -0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("unsupported CVS date %q", s)
}
