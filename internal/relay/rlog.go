package relay

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"cvsgitrelay/internal/diag"
)

const revisionSeparator = "----------------------------"
const fileSeparator = "============================================================================="

var countsPattern = regexp.MustCompile(`^total revisions: ([0-9]+);[ \t]+selected revisions: ([0-9]+)$`)
var linesPattern = regexp.MustCompile(`^lines: \+[0-9]+ -[0-9]+$`)

// parseRlog recognizes the demonstrated GNU CVS grammar. A separator in a log
// message is data unless it also impersonates a complete structural boundary.
// Such extra records fail the declared count and uniqueness checks.
func parseRlog(data []byte, prefix string) (*Inventory, error) {
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, diag.New("parse", "rlog", "NUL in metadata output", nil)
	}
	lines := strings.Split(string(data), "\n")
	var files []*File
	for pos := 0; pos < len(lines); {
		if lines[pos] == "" {
			pos++
			continue
		}
		start := pos
		if !strings.HasPrefix(lines[pos], "RCS file: ") {
			return nil, parseError(pos, "expected RCS file header")
		}
		rcs := strings.TrimPrefix(lines[pos], "RCS file: ")
		if !strings.HasPrefix(rcs, prefix) || !strings.HasSuffix(rcs, ",v") {
			return nil, parseError(pos, "RCS path does not match literal CVSROOT/module mapping; aliases and remapped roots are unsupported")
		}
		p := strings.TrimSuffix(strings.TrimPrefix(rcs, prefix), ",v")
		parts := strings.Split(p, "/")
		if len(parts) > 1 && parts[len(parts)-2] == "Attic" {
			parts = append(parts[:len(parts)-2], parts[len(parts)-1])
			p = strings.Join(parts, "/")
		}
		if err := validatePath(p); err != nil {
			return nil, err
		}
		f := &File{Path: p, Symbols: map[string]string{}}
		pos++
		take := func(prefix string) (string, bool) {
			if pos >= len(lines) || !strings.HasPrefix(lines[pos], prefix) {
				return "", false
			}
			s := strings.TrimSpace(strings.TrimPrefix(lines[pos], prefix))
			pos++
			return s, true
		}
		var ok bool
		if f.Head, ok = take("head: "); !ok {
			return nil, parseError(pos, "expected head")
		}
		if f.DefaultBranch, ok = take("branch:"); !ok {
			return nil, parseError(pos, "expected default branch")
		}
		if f.DefaultBranch != "" {
			if b, ok := branchNumber(f.DefaultBranch); !ok || b != f.DefaultBranch {
				return nil, parseError(pos, "invalid default branch number")
			}
		}
		if _, ok = take("locks:"); !ok {
			return nil, parseError(pos, "expected locks")
		}
		for pos < len(lines) && strings.HasPrefix(lines[pos], "\t") {
			pos++
		}
		if _, ok = take("access list:"); !ok {
			return nil, parseError(pos, "expected access list")
		}
		for pos < len(lines) && strings.HasPrefix(lines[pos], "\t") {
			pos++
		}
		if _, ok = take("symbolic names:"); !ok {
			return nil, parseError(pos, "expected symbolic names")
		}
		for pos < len(lines) && strings.HasPrefix(lines[pos], "\t") {
			name, num, found := strings.Cut(strings.TrimPrefix(lines[pos], "\t"), ": ")
			if _, err := numbers(num); !found || err != nil || name == "" || strings.ContainsAny(name, "\x00\r\n\t") {
				return nil, parseError(pos, "invalid symbolic name")
			}
			n, _ := numbers(num)
			if slices.Contains(n, 0) {
				if _, ok := branchNumber(num); !ok {
					return nil, parseError(pos, "invalid magic branch symbol")
				}
			}
			if _, exists := f.Symbols[name]; exists {
				return nil, parseError(pos, "duplicate symbolic name")
			}
			f.Symbols[name] = num
			pos++
		}
		if _, ok = take("keyword substitution:"); !ok {
			return nil, parseError(pos, "expected keyword mode")
		}
		if pos >= len(lines) {
			return nil, parseError(pos, "missing revision counts")
		}
		m := countsPattern.FindStringSubmatch(lines[pos])
		if m == nil {
			return nil, parseError(pos, "invalid total/selected counts")
		}
		total, _ := strconv.Atoi(m[1])
		selected, _ := strconv.Atoi(m[2])
		pos++
		if total < 1 || total != selected {
			return nil, parseError(pos, "empty or filtered history; total and selected counts must match")
		}
		if _, ok = take("description:"); !ok {
			return nil, parseError(pos, "expected description")
		}
		for pos < len(lines) && !recordBoundary(lines, pos) && lines[pos] != fileSeparator {
			pos++
		}
		for pos < len(lines) && recordBoundary(lines, pos) {
			pos++
			number := strings.TrimPrefix(lines[pos], "revision ")
			pos++
			r := &Revision{Number: number}
			if _, err := numbers(number); err != nil {
				return nil, parseError(pos, "invalid numeric revision")
			}
			if pos >= len(lines) {
				return nil, parseError(pos, "missing revision metadata")
			}
			if err := parseMetadata(lines[pos], r); err != nil {
				return nil, diag.New("parse", "rlog.metadata", "unsupported revision record", err, "line", pos+1, "path_hex", hexBytes(p), "revision", number)
			}
			pos++
			if pos < len(lines) && strings.HasPrefix(lines[pos], "branches:") {
				for _, b := range strings.Split(strings.TrimPrefix(lines[pos], "branches:"), ";") {
					if b = strings.TrimSpace(b); b == "" {
						continue
					}
					if _, ok := branchNumber(b); !ok {
						return nil, parseError(pos, "invalid branches metadata")
					}
				}
				pos++
			}
			logStart := pos
			for pos < len(lines) && !recordBoundary(lines, pos) && !fileBoundary(lines, pos) {
				pos++
			}
			// rlog adds a newline after each log. Preserve the exposed bytes,
			// including the final newline and all separator-like message lines.
			r.Log = []byte(strings.Join(lines[logStart:pos], "\n") + "\n")
			f.Revisions = append(f.Revisions, r)
		}
		if pos >= len(lines) || !fileBoundary(lines, pos) {
			return nil, parseError(pos, "truncated file block")
		}
		pos++
		if len(f.Revisions) != total {
			return nil, diag.New("parse", "rlog.count", "revision count mismatch or ambiguous log boundary", nil, "path_hex", hexBytes(p), "line", start+1, "declared", total, "parsed", len(f.Revisions))
		}
		files = append(files, f)
	}
	return buildInventory(files)
}

func recordBoundary(lines []string, i int) bool {
	return i+2 < len(lines) && lines[i] == revisionSeparator && strings.HasPrefix(lines[i+1], "revision ") && strings.HasPrefix(lines[i+2], "date: ")
}

func fileBoundary(lines []string, i int) bool {
	if lines[i] != fileSeparator {
		return false
	}
	j := i + 1
	for j < len(lines) && lines[j] == "" {
		j++
	}
	return j == len(lines) || strings.HasPrefix(lines[j], "RCS file: ")
}

func parseMetadata(s string, r *Revision) error {
	parts := strings.Split(s, ";")
	if len(parts) < 4 {
		return fmt.Errorf("missing mandatory metadata fields")
	}
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, ": ")
		if !ok || seen[name] {
			return fmt.Errorf("ambiguous metadata field")
		}
		seen[name] = true
		switch name {
		case "date":
			ts, err := parseDate(value)
			if err != nil {
				return err
			}
			r.Time = ts
		case "author":
			if value == "" || strings.ContainsAny(value, "\x00\r\n") {
				return fmt.Errorf("invalid author")
			}
			r.Author = value
		case "state":
			if value != "Exp" && value != "dead" {
				return fmt.Errorf("unsupported revision state")
			}
			r.State = value
		case "lines":
			if !linesPattern.MatchString(part) {
				return fmt.Errorf("invalid lines metadata")
			}
		case "commitid":
			if value == "" || strings.ContainsAny(value, " \t\r\n\x00") {
				return fmt.Errorf("invalid commitid")
			}
			r.CommitID = value
		default:
			return fmt.Errorf("unknown metadata field %q", name)
		}
	}
	if !seen["date"] || !seen["author"] || !seen["state"] {
		return fmt.Errorf("missing date, author or state")
	}
	return nil
}

func parseError(line int, message string) error {
	return diag.New("parse", "rlog.parse", message, nil, "line", line+1)
}
