package canonical

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// Growing history commits CVS changesets: the revisions of one CVS commit
// share a commit. CVS records commits per file, so the revisions of a
// changeset are those of one line with the same author and log message and
// either the same CVS commit ID or, for revisions without one, times close
// to each other.

// changesetWindow is the largest gap in seconds between two revisions without
// a commit ID that still belong to one changeset. CVS commits one file after
// the other, which takes a while for large commits over slow connections.
const changesetWindow = 300

// Trailer lines that only changeset messages have: a dead revision, which
// removes its file, and the CVS commit ID shared by all revisions.
const (
	trailerDead     = "CVS-Dead: "
	trailerCommitID = "CVS-Commitid: "
)

// changeset is one commit of growing history in the making.
type changeset struct {
	index    int
	members  []Record
	archives map[string]bool
	last     int64
}

// changesetSignature identifies the revisions that may share a changeset.
func changesetSignature(r Record) string {
	return string(append(append(frame(r.Author), frame(string(r.Log))...), frame(r.CommitID)...))
}

// groupChangesets orders the new revisions of one line and groups them into
// changesets. Revisions are visited in the same order as the events of
// canonical history: the earliest ready revision by eventCompare, where a
// revision is ready once its parent is published or visited. A revision
// joins the latest changeset of its signature unless that already holds its
// file, its parent was grouped into that changeset or a later one, or, for
// revisions without a commit ID, it is more than changesetWindow seconds
// away from the changeset's newest revision. Otherwise it starts a new
// changeset. Changesets are committed in the order they were started, so
// every revision still follows its parent.
func groupChangesets(pending []Record, published map[string]string) ([]*changeset, error) {
	slices.SortFunc(pending, eventCompare)
	var queue readyQueue
	waiting := map[string][]int{}
	for i, r := range pending {
		if r.Parent == nil || published[r.Parent.ID()] != "" {
			queue.push(i)
		} else {
			waiting[r.Parent.ID()] = append(waiting[r.Parent.ID()], i)
		}
	}
	var groups []*changeset
	latest := map[string]int{}
	groupOf := map[string]int{}
	for range pending {
		if queue.Len() == 0 {
			return nil, fmt.Errorf("cyclic ancestry")
		}
		r := pending[queue.pop()]
		signature := changesetSignature(r)
		var g *changeset
		if i, ok := latest[signature]; ok {
			g = groups[i]
			parentGroup, grouped := -1, false
			if r.Parent != nil {
				parentGroup, grouped = groupOf[r.Parent.ID()]
			}
			gap := max(r.Time-g.last, g.last-r.Time)
			if g.archives[r.Key.Archive] || (grouped && parentGroup >= g.index) || (r.CommitID == "" && gap > changesetWindow) {
				g = nil
			}
		}
		if g == nil {
			g = &changeset{index: len(groups), archives: map[string]bool{}, last: r.Time}
			groups = append(groups, g)
			latest[signature] = g.index
		}
		g.members = append(g.members, r)
		g.archives[r.Key.Archive] = true
		g.last = max(g.last, r.Time)
		groupOf[r.Key.ID()] = g.index
		for _, child := range waiting[r.Key.ID()] {
			queue.push(child)
		}
		delete(waiting, r.Key.ID())
	}
	return groups, nil
}

// time is the commit time of a changeset: that of its newest revision.
func (c *changeset) time() int64 {
	t := c.members[0].Time
	for _, r := range c.members[1:] {
		t = max(t, r.Time)
	}
	return t
}

// apply changes the tree as the changeset's revisions do. Removals come
// first, so that a file can replace a removed directory and the other way
// round.
func (c *changeset) apply(files *lineTree, blobs map[string]string) error {
	members := slices.Clone(c.members)
	slices.SortFunc(members, func(a, b Record) int {
		return cmp.Or(-cmp.Compare(boolRank(a.State == "dead"), boolRank(b.State == "dead")), strings.Compare(a.Path, b.Path))
	})
	for _, r := range members {
		if r.State == "dead" {
			files.remove(r.Path)
		} else if err := files.set(r.Path, blobs[r.Key.ID()], r.Executable); err != nil {
			return err
		}
	}
	return nil
}

// changesetMessage puts the shared CVS log first, so Git shows it as the
// subject and body, then a blank line and one trailer line per revision,
// sorted by archive: "CVS: <revision> <archive>" for a revision that leaves
// the file in place and "CVS-Dead: <revision> <archive>" for one that removes
// it. The optional lines follow for a mapped author, a CVS commit ID and a
// log that cannot be shown verbatim.
func (p Policy) changesetMessage(members []Record) []byte {
	first := members[0]
	shown, logValue := shownLog(first.Log)
	var b bytes.Buffer
	b.WriteString(shown)
	if !strings.HasSuffix(shown, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	sorted := slices.Clone(members)
	slices.SortFunc(sorted, func(a, b Record) int { return strings.Compare(a.Key.Archive, b.Key.Archive) })
	for _, r := range sorted {
		prefix := trailerRevision
		if r.State == "dead" {
			prefix = trailerDead
		}
		fmt.Fprintf(&b, "%s%s %s\n", prefix, r.Key.Number, textField(r.Key.Archive))
	}
	if _, mapped := p.Authors[first.Author]; mapped {
		fmt.Fprintf(&b, "%s%s\n", trailerAuthor, textField(first.Author))
	}
	if first.CommitID != "" {
		fmt.Fprintf(&b, "%s%s\n", trailerCommitID, textField(first.CommitID))
	}
	if logValue != "" {
		fmt.Fprintf(&b, "%s%s\n", trailerLog, logValue)
	}
	return b.Bytes()
}

// ParseChangeset recovers the revisions of a changeset commit from its Git
// author identity and its message: their keys and states, and the author,
// log and commit ID they share. Times are not recoverable, since the commit
// has only one. The message must be exactly what changesetMessage writes for
// the recovered revisions.
func (p Policy) ParseChangeset(identity string, message []byte) ([]Record, error) {
	i := bytes.LastIndex(message, []byte("\n\n"))
	if i < 0 || !bytes.HasSuffix(message, []byte("\n")) {
		return nil, fmt.Errorf("invalid changeset message")
	}
	body, rest := message[:i+1], strings.Split(strings.TrimSuffix(string(message[i+2:]), "\n"), "\n")
	var members []Record
	archives := map[string]bool{}
	for len(rest) > 0 {
		state := "live"
		value, ok := strings.CutPrefix(rest[0], trailerRevision)
		if !ok {
			if value, ok = strings.CutPrefix(rest[0], trailerDead); !ok {
				break
			}
			state = "dead"
		}
		number, field, ok := strings.Cut(value, " ")
		archive, err := parseTextField(field)
		if !ok || err != nil || archives[archive] {
			return nil, fmt.Errorf("invalid changeset revision line")
		}
		archives[archive] = true
		members = append(members, Record{Key: Key{Source: p.Source, Archive: archive, Number: number}, State: state})
		rest = rest[1:]
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("changeset without revisions")
	}
	var author, commitID string
	var err error
	if len(rest) > 0 && strings.HasPrefix(rest[0], trailerAuthor) {
		if author, err = parseTextField(strings.TrimPrefix(rest[0], trailerAuthor)); err != nil {
			return nil, err
		}
		rest = rest[1:]
	} else if author, err = decodeIdentity(identity); err != nil {
		return nil, err
	}
	if len(rest) > 0 && strings.HasPrefix(rest[0], trailerCommitID) {
		if commitID, err = parseTextField(strings.TrimPrefix(rest[0], trailerCommitID)); err != nil || !validCommitID(commitID) {
			return nil, fmt.Errorf("invalid commit ID trailer")
		}
		rest = rest[1:]
	}
	log := slices.Clone(body)
	if len(rest) > 0 && strings.HasPrefix(rest[0], trailerLog) {
		value := strings.TrimPrefix(rest[0], trailerLog)
		rest = rest[1:]
		if value == logNoNewline {
			log = log[:len(log)-1]
		} else if h, ok := strings.CutPrefix(value, hexPrefix); ok {
			if log, err = hex.DecodeString(h); err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("invalid log trailer")
		}
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("unrecognized trailer lines")
	}
	for i := range members {
		members[i].Author, members[i].Log, members[i].CommitID = author, log, commitID
		if err := validRevisionIdentity(members[i]); err != nil {
			return nil, err
		}
	}
	if p.EventAuthor(author) != identity || !bytes.Equal(p.changesetMessage(members), message) {
		return nil, fmt.Errorf("noncanonical changeset message")
	}
	return members, nil
}
