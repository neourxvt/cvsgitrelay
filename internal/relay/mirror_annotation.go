package relay

import (
	"bytes"
	"fmt"
	"regexp"

	"cvsgitrelay/internal/diag"
)

var annotationPrefix = regexp.MustCompile(`^([0-9.]+) +\(.*? [0-9]{2}-[A-Za-z]{3}-[0-9]{2,4}\): `)

// CVS annotation retains binary bytes but appends a newline to an unterminated
// final line. Live checkouts supply that last bit; for dead revisions the raw
// annotation is retained too, preserving all observable source information.
func parseMirrorAnnotation(raw []byte) ([]byte, []string, error) {
	var content []byte
	var origins []string
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, '\n')
		if end < 0 {
			return nil, nil, diag.New("parse", "mirror.annotate", "unterminated annotation record", nil)
		}
		line := raw[:end+1]
		raw = raw[end+1:]
		match := annotationPrefix.FindSubmatchIndex(line)
		if match == nil {
			return nil, nil, diag.New("parse", "mirror.annotate", "unsupported annotation framing", nil)
		}
		origins = append(origins, string(line[match[2]:match[3]]))
		content = append(content, line[match[1]:]...)
	}
	return content, origins, nil
}

func fileLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	lines := bytes.SplitAfter(data, []byte{'\n'})
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// The newer revision owns every line inserted by an adjacent CVS change.
// All other lines survive from its predecessor in order. Matching that stream
// directly gives deterministic deltas without external diff implementation or
// ambiguous LCS tie-breaking, including repeated identical source lines.
func historyDelta(old, next []byte, oldOrigins, nextOrigins []string, change string, forward bool) ([]byte, error) {
	oldLines, nextLines := fileLines(old), fileLines(next)
	if len(oldLines) != len(oldOrigins) || len(nextLines) != len(nextOrigins) {
		return nil, diag.New("integrity", "mirror.delta", "annotation and content line counts differ", nil)
	}
	type match struct{ old, next int }
	var matches []match
	if forward {
		cursor := 0
		for j, origin := range nextOrigins {
			if origin == change {
				continue
			}
			for cursor < len(oldLines) && (oldOrigins[cursor] != origin || !bytes.Equal(oldLines[cursor], nextLines[j])) {
				cursor++
			}
			if cursor == len(oldLines) {
				return nil, diag.New("integrity", "mirror.delta", "branch annotation has no matching predecessor line", nil)
			}
			matches = append(matches, match{cursor, j})
			cursor++
		}
	} else {
		cursor := 0
		for i, origin := range oldOrigins {
			if origin == change {
				continue
			}
			for cursor < len(nextLines) && (nextOrigins[cursor] != origin || !bytes.Equal(oldLines[i], nextLines[cursor])) {
				cursor++
			}
			if cursor == len(nextLines) {
				return nil, diag.New("integrity", "mirror.delta", "trunk annotation has no matching predecessor line", nil)
			}
			matches = append(matches, match{i, cursor})
			cursor++
		}
	}
	matches = append(matches, match{len(oldLines), len(nextLines)})
	var delta bytes.Buffer
	oldCursor, newCursor := 0, 0
	for _, point := range matches {
		if count := point.old - oldCursor; count > 0 {
			fmt.Fprintf(&delta, "d%d %d\n", oldCursor+1, count)
		}
		if count := point.next - newCursor; count > 0 {
			fmt.Fprintf(&delta, "a%d %d\n", point.old, count)
			for _, line := range nextLines[newCursor:point.next] {
				delta.Write(line)
			}
		}
		oldCursor, newCursor = point.old+1, point.next+1
	}
	return delta.Bytes(), nil
}
