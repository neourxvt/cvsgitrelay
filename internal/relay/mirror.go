package relay

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"cvsgitrelay/internal/diag"
)

// RCS identifiers are unquoted, unlike log messages and file contents. Reject
// delimiters rather than allowing source metadata to inject archive syntax.
func rcsIdentifier(value string) bool {
	return value != "" && !strings.ContainsAny(value, " \t\r\n\v\f$,:;@\x00")
}

// Buffered writes remember the first I/O error; the archive caller checks
// Flush. Escaping incrementally avoids another copy of a complete revision.
func writeRCSString(out *bufio.Writer, data []byte) {
	out.WriteByte('@')
	for {
		at := bytes.IndexByte(data, '@')
		if at < 0 {
			out.Write(data)
			break
		}
		out.Write(data[:at+1])
		out.WriteByte('@')
		data = data[at+1:]
	}
	out.WriteByte('@')
}

func (m *cvsMirror) writeFile(ctx context.Context, root string, file *File, connections int) error {
	// The head stores full text. Trunk deltas run backwards; branch deltas run
	// forwards from the forkpoint. Bodies stay on disk between adjacent pairs.
	revisions := slices.Clone(file.Revisions)
	slices.SortFunc(revisions, func(a, b *Revision) int {
		if a.Line == "trunk" && b.Line == "trunk" {
			return -compareNumber(a.Number, b.Number)
		}
		if a.Line == "trunk" {
			return -1
		}
		if b.Line == "trunk" {
			return 1
		}
		// CVS reads deltatexts forwards, so every branch's forkpoint and
		// earlier revisions must precede its own replacement scripts.
		return compareNumber(a.Number, b.Number)
	})
	m.progress.update("activity", "Reading revision contents")
	if err := m.cacheContents(ctx, revisions, connections); err != nil {
		return err
	}
	m.progress.update("activity", "Building CVS archive")
	branches := map[string][]string{}
	next := map[string]string{}
	base := map[string]string{}
	for _, revision := range revisions {
		if !rcsIdentifier(revision.Author) || (revision.CommitID != "" && !rcsIdentifier(revision.CommitID)) {
			return diag.New("unsupported", "mirror.rcs", "revision metadata cannot be represented as RCS identifiers", nil, "path_hex", hexBytes(file.Path), "revision", revision.Number)
		}
		if revision.Line == "trunk" {
			next[revision.Number] = revision.Predecessor
			if revision.Predecessor != "" {
				base[revision.Predecessor] = revision.Number
			}
		} else {
			base[revision.Number] = revision.Predecessor
			if strings.HasPrefix(revision.Predecessor, revision.Line+".") {
				next[revision.Predecessor] = revision.Number
			} else {
				branches[revision.Predecessor] = append(branches[revision.Predecessor], revision.Number)
			}
		}
	}
	// CVS annotation walks branch deltatexts at their forkpoint before resuming
	// the older parent line. A merely numeric ordering can serve checkout while
	// causing annotation to reach EOF when it returns to the trunk ancestry.
	byNumber := map[string]*Revision{}
	for _, revision := range revisions {
		byNumber[revision.Number] = revision
	}
	var ordered []*Revision
	var visit func(string)
	visit = func(number string) {
		if number == "" {
			return
		}
		ordered = append(ordered, byNumber[number])
		slices.SortFunc(branches[number], compareNumber)
		for _, branch := range branches[number] {
			visit(branch)
		}
		visit(next[number])
	}
	visit(file.Head)
	revisions = ordered
	path := filepath.Join(root, mirrorArchivePath(file))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := mirrorWriteStream(ctx, path, func(out io.Writer) error {
		archive := bufio.NewWriter(out)
		fmt.Fprintf(archive, "head %s;\nbranch %s;\naccess", file.Head, file.DefaultBranch)
		for _, owner := range file.Access {
			fmt.Fprintf(archive, "\n\t%s", owner)
		}
		archive.WriteString(";\nsymbols")
		names := make([]string, 0, len(file.Symbols))
		for name := range file.Symbols {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if !rcsIdentifier(name) {
				return diag.New("unsupported", "mirror.rcs", "symbol cannot be represented as an RCS identifier", nil, "symbol_hex", hexBytes(name))
			}
			fmt.Fprintf(archive, "\n\t%s:%s", name, file.Symbols[name])
		}
		archive.WriteString(";\nlocks")
		var locked []string
		for revision := range file.Locks {
			locked = append(locked, revision)
		}
		slices.SortFunc(locked, compareNumber)
		for _, revision := range locked {
			fmt.Fprintf(archive, "\n\t%s:%s", file.Locks[revision], revision)
		}
		archive.WriteByte(';')
		if file.StrictLocks {
			archive.WriteString(" strict;")
		}
		archive.WriteString("\ncomment @# @;\nexpand ")
		writeRCSString(archive, []byte(file.KeywordMode))
		archive.WriteString(";\n\n")
		for _, revision := range revisions {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintf(archive, "%s\ndate %s; author %s; state %s;\nbranches", revision.Number, time.Unix(revision.Time, 0).UTC().Format("2006.01.02.15.04.05"), revision.Author, revision.State)
			slices.SortFunc(branches[revision.Number], compareNumber)
			for _, branch := range branches[revision.Number] {
				fmt.Fprintf(archive, "\n\t%s", branch)
			}
			fmt.Fprintf(archive, ";\nnext %s;\n", next[revision.Number])
			if revision.CommitID != "" {
				fmt.Fprintf(archive, "commitid %s;\n", revision.CommitID)
			}
			archive.WriteByte('\n')
		}
		archive.WriteString("desc\n")
		writeRCSString(archive, file.Description)
		archive.WriteByte('\n')
		for _, revision := range revisions {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintf(archive, "\n%s\nlog\n", revision.Number)
			writeRCSString(archive, revision.Log)
			archive.WriteString("\ntext\n")
			data, err := m.revisionDelta(revision, byNumber[base[revision.Number]])
			if err != nil {
				return err
			}
			writeRCSString(archive, data)
			archive.WriteByte('\n')
			if err := archive.Flush(); err != nil {
				return err
			}
		}
		return archive.Flush()
	}); err != nil {
		return err
	}
	if err := os.Chmod(path, m.modes[file.SourcePath]); err != nil {
		return err
	}
	// Validate reconstructed history with CVS, reloading the expected revision
	// from the durable cache instead of retaining every expected body in memory.
	local := &Repository{Config: Config{CVSRoot: root, Module: m.source.Config.Module}, Runner: m.source.Runner}
	m.progress.update("activity", "Verifying generated revisions")
	for _, revision := range revisions {
		m.progress.update("revision", revision.Number)
		entry, err := m.readEntry(revision)
		if err != nil {
			return err
		}
		raw, err := local.cvs(ctx, "", "rannotate", "-F", "-r", revision.Number, revision.SourcePath)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, entry.Annotation) {
			return diag.New("integrity", "mirror.compare", "generated line history differs from source annotation", nil, "path_hex", hexBytes(file.Path), "revision", revision.Number)
		}
		if revision.State == "dead" {
			continue
		}
		copied, err := local.fetch(ctx, revision)
		if err != nil {
			return err
		}
		expected, err := m.readContent(revision)
		if err != nil {
			return err
		}
		if !bytes.Equal(expected, copied) {
			return diag.New("integrity", "mirror.compare", "generated CVS revision contents differ from the source", nil, "path_hex", hexBytes(file.Path), "revision", revision.Number)
		}
	}
	return nil
}
