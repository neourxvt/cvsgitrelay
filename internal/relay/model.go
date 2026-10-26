package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

type Revision struct {
	Path        string
	SourcePath  string
	Number      string
	Author      string
	State       string
	Time        int64
	Log         []byte
	CommitID    string
	Locker      string
	Lines       string
	Branches    []string
	Line        string
	Predecessor string
	Ref         string
}

type File struct {
	Path          string
	SourcePath    string
	Head          string
	DefaultBranch string
	KeywordMode   string
	Description   []byte
	Symbols       map[string]string
	StrictLocks   bool
	Locks         map[string]string
	Access        []string
	InAttic       bool
	Revisions     []*Revision
}

type Inventory struct {
	Layout            []ModuleMapping
	Submodules        string
	Excluded          []ModuleMapping
	ExcludedFiles     int
	ExcludedRevisions int
	Files             map[string]*File
	Revisions         map[string]*Revision
	Ordered           []*Revision
	Branches          map[string]map[string]string // destination -> path -> forkpoint
	Trunk             int
	Dead              int
	Vendor            int
	NumberingGaps     int // skipped numeric ranges, not missing parsed records
}

func key(path, number string) string { return path + "\x00" + number }
func hexBytes(s string) string       { return hex.EncodeToString([]byte(s)) }
func digest(data []byte) string      { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func numbers(s string) ([]uint64, error) { return canonical.Number(s) }

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
		parts[i] = strconv.FormatUint(v, 10)
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

func parseDate(s string) (int64, error) {
	for _, layout := range []string{"2006/01/02 15:04:05", "2006-01-02 15:04:05 -0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("unsupported CVS date %q", s)
}
