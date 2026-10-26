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

type rlogMapping struct {
	RCSPrefix, SourcePrefix, DestinationPrefix string
	Excluded                                   bool
}

// Both importers share the exposed GNU CVS grammar; the CVS mirror additionally
// preserves administrative fields and states outside the canonical Git model.
func parseMappedRlogMode(data []byte, mappings []rlogMapping, mirror bool) (*Inventory, error) {
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, diag.New("parse", "rlog", "NUL in metadata output", nil)
	}
	lines := strings.Split(string(data), "\n")
	var files []*File
	var excluded []ModuleMapping
	for _, mapping := range mappings {
		if mapping.Excluded {
			excluded = append(excluded, ModuleMapping{Source: mapping.SourcePrefix, Destination: mapping.DestinationPrefix})
		}
	}
	excludedFiles, excludedRevisions := 0, 0
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
		var mapping *rlogMapping
		var prefixes []string
		for i := range mappings {
			prefixes = append(prefixes, mappings[i].RCSPrefix)
			if strings.HasPrefix(rcs, mappings[i].RCSPrefix) {
				if mapping != nil {
					return nil, parseError(pos, "RCS file matches multiple module directories")
				}
				mapping = &mappings[i]
			}
		}
		if mapping == nil || !strings.HasSuffix(rcs, ",v") {
			return nil, diag.New("parse", "rlog.parse", "RCS path does not match the configured module directory", nil,
				"line", pos+1, "actual_rcs_path", rcs, "expected_rcs_prefixes", prefixes,
				"hint", "file is outside the directories declared by the resolved CVS module")
		}
		p := strings.TrimSuffix(strings.TrimPrefix(rcs, mapping.RCSPrefix), ",v")
		inAttic := false
		parts := strings.Split(p, "/")
		if len(parts) > 1 && parts[len(parts)-2] == "Attic" {
			inAttic = true
			parts = append(parts[:len(parts)-2], parts[len(parts)-1])
			p = strings.Join(parts, "/")
		}
		if err := validatePath(p); err != nil {
			return nil, err
		}
		sourcePath := ""
		if mapping.SourcePrefix != "" {
			sourcePath = mapping.SourcePrefix + "/" + p
		}
		if mapping.DestinationPrefix != "" {
			p = mapping.DestinationPrefix + "/" + p
		}
		if err := validatePath(p); err != nil {
			return nil, err
		}
		f := &File{Path: p, SourcePath: sourcePath, Symbols: map[string]string{}, Locks: map[string]string{}, InAttic: inAttic}
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
		locking, ok := take("locks:")
		if !ok {
			return nil, parseError(pos, "expected locks")
		}
		f.StrictLocks = locking == "strict"
		for pos < len(lines) && strings.HasPrefix(lines[pos], "\t") {
			owner, revision, found := strings.Cut(strings.TrimSpace(lines[pos]), ": ")
			if _, err := numbers(revision); !found || err != nil || !rcsIdentifier(owner) {
				return nil, parseError(pos, "invalid revision lock")
			}
			f.Locks[revision] = owner
			pos++
		}
		if _, ok = take("access list:"); !ok {
			return nil, parseError(pos, "expected access list")
		}
		for pos < len(lines) && strings.HasPrefix(lines[pos], "\t") {
			owner := strings.TrimSpace(lines[pos])
			if !rcsIdentifier(owner) {
				return nil, parseError(pos, "invalid access-list entry")
			}
			f.Access = append(f.Access, owner)
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
			if slices.Contains(n, uint64(0)) {
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
		if f.KeywordMode, ok = take("keyword substitution:"); !ok {
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
		if total != selected {
			return nil, parseError(pos, "filtered history; total and selected counts must match")
		}
		if _, ok = take("description:"); !ok {
			return nil, parseError(pos, "expected description")
		}
		descriptionStart := pos
		for pos < len(lines) && !recordBoundary(lines, pos) && lines[pos] != fileSeparator {
			pos++
		}
		if pos > descriptionStart {
			f.Description = []byte(strings.Join(lines[descriptionStart:pos], "\n") + "\n")
		}
		for pos < len(lines) && recordBoundary(lines, pos) {
			pos++
			number := strings.TrimPrefix(lines[pos], "revision ")
			number, lockedBy, locked := strings.Cut(number, "\tlocked by: ")
			locker := ""
			if locked {
				if !strings.HasSuffix(lockedBy, ";") {
					return nil, parseError(pos, "invalid revision locker")
				}
				locker = strings.TrimSuffix(lockedBy, ";")
				if !rcsIdentifier(locker) {
					return nil, parseError(pos, "invalid revision locker")
				}
			}
			pos++
			r := &Revision{Number: number, Locker: locker}
			if _, err := numbers(number); err != nil {
				return nil, parseError(pos, "invalid numeric revision")
			}
			if pos >= len(lines) {
				return nil, parseError(pos, "missing revision metadata")
			}
			if err := parseMetadataMode(lines[pos], r, mirror); err != nil {
				return nil, diag.New("parse", "rlog.metadata", "unsupported revision record", err, "line", pos+1, "path_hex", hexBytes(p), "revision", number)
			}
			pos++
			if pos < len(lines) && strings.HasPrefix(lines[pos], "branches:") {
				for _, b := range strings.Split(strings.TrimPrefix(lines[pos], "branches:"), ";") {
					if b = strings.TrimSpace(b); b == "" {
						continue
					}
					r.Branches = append(r.Branches, b)
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
			r.Log = []byte(strings.Join(lines[logStart:pos], "\n"))
			if pos > logStart {
				r.Log = append(r.Log, '\n')
			}
			f.Revisions = append(f.Revisions, r)
		}
		if pos >= len(lines) || !fileBoundary(lines, pos) {
			return nil, parseError(pos, "truncated file block")
		}
		pos++
		if len(f.Revisions) != total {
			return nil, diag.New("parse", "rlog.count", "revision count mismatch or ambiguous log boundary", nil, "path_hex", hexBytes(p), "line", start+1, "declared", total, "parsed", len(f.Revisions))
		}
		// Validate framing and declared counts even for excluded modules, then
		// omit their files before building revision ancestry or branch seeds.
		// A module also owns its destination if the main source overlaps it.
		if mapping.Excluded || excludedPath(p, excluded) {
			excludedFiles++
			excludedRevisions += len(f.Revisions)
		} else {
			files = append(files, f)
		}
	}
	in, err := buildFileInventory(files, mirror)
	if err != nil {
		return nil, err
	}
	in.Excluded = excluded
	in.ExcludedFiles, in.ExcludedRevisions = excludedFiles, excludedRevisions
	return in, nil
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

func parseMetadataMode(s string, r *Revision, mirror bool) error {
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
		if mirror && part == "state:" {
			name, value, ok = "state", "", true
		}
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
			if (!mirror && value != "Exp" && value != "dead") || (mirror && value != "" && !rcsIdentifier(value)) {
				return fmt.Errorf("unsupported revision state")
			}
			r.State = value
		case "lines":
			r.Lines = value
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
