package relay

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"

	"cvsgitrelay/internal/diag"
)

// Read full cache records only while using a revision. The compact index also
// lets us reject a record changed after the initial cache validation.
func (m *cvsMirror) readEntry(ctx context.Context, revision *Revision) (mirrorEntry, error) {
	var entry mirrorEntry
	id := mirrorKey(revision)
	if indexed := m.entries[id]; indexed.Archived {
		raw, err := m.archiveRepository(revision.SourcePath).cvs(ctx, "", "rannotate", "-F", "-r", revision.Number, revision.SourcePath)
		if err != nil {
			return entry, err
		}
		if digest(raw) != indexed.AnnotationHash {
			return entry, diag.New("integrity", "mirror.archive", "local annotation digest mismatch", nil)
		}
		return mirrorEntry{Revision: mirrorMetadata(revision), Blob: indexed.Blob, Annotated: true, Annotation: raw}, nil
	}
	data, err := mirrorReadCompressed(filepath.Join(m.state, "revisions", id+".json"))
	if err != nil {
		return entry, err
	}
	if err := decodeJSON(data, &entry); err != nil {
		return entry, diag.New("integrity", "mirror.cache", "invalid cached revision metadata", err)
	}
	entry.Revision.Path = entry.Revision.SourcePath
	indexed, exists := m.entries[id]
	if !exists || entry.Blob != indexed.Blob || entry.Annotated != indexed.Annotated || (entry.Annotated && digest(entry.Annotation) != indexed.AnnotationHash) || !reflect.DeepEqual(entry.Revision, mirrorMetadata(revision)) {
		return entry, diag.New("integrity", "mirror.cache", "cached revision changed after validation", nil, "path_hex", hexBytes(revision.Path), "revision", revision.Number)
	}
	return entry, nil
}

// Every body read is checked against its durable digest, including bodies
// reloaded as branch forkpoints or during local archive verification.
func (m *cvsMirror) readContent(ctx context.Context, revision *Revision) ([]byte, error) {
	entry := m.entries[mirrorKey(revision)]
	if entry.Archived {
		_, data, err := m.archivedRevision(ctx, revision)
		return data, err
	}
	data, err := mirrorReadBlob(filepath.Join(m.state, "blobs", entry.Blob))
	if err != nil {
		return nil, err
	}
	if digest(data) != entry.Blob {
		return nil, diag.New("integrity", "mirror.cache", "cached content digest mismatch", nil)
	}
	return data, nil
}

// Pairwise delta construction needs only these parsed origins and the exact
// body. Nothing returned here is retained in the repository-wide cache index.
func (m *cvsMirror) readRevision(ctx context.Context, revision *Revision) ([]byte, []string, error) {
	entry, err := m.readEntry(ctx, revision)
	if err != nil {
		return nil, nil, err
	}
	_, origins, err := parseMirrorAnnotation(entry.Annotation)
	if err != nil {
		return nil, nil, err
	}
	var data []byte
	if m.entries[mirrorKey(revision)].Archived {
		data, err = m.annotationContent(revision, entry.Annotation)
	} else {
		data, err = m.readContent(ctx, revision)
	}
	return data, origins, err
}

// The origin and digest identify an unterminated final line without keeping
// its bytes alive. Only these compact hints survive the acquisition pass.
type mirrorLineEnd struct {
	origin string
	digest string
}

func mirrorLastLine(data []byte) []byte {
	return data[bytes.LastIndexByte(data, '\n')+1:]
}

// Fill the durable cache before constructing deltas. Acquisition retains one
// revision at a time, plus the existing bounded annotation read-ahead window.
func (m *cvsMirror) cacheContents(ctx context.Context, revisions []*Revision, connections int) error {
	var missing []*Revision
	for _, rev := range revisions {
		entry, exists := m.entries[mirrorKey(rev)]
		if !exists || !entry.Annotated {
			missing = append(missing, rev)
		}
	}
	fetches := newAllRevisionFetches(ctx, missing, connections, func(ctx context.Context, rev *Revision) ([]byte, error) {
		return m.source.cvs(ctx, "", "rannotate", "-F", "-r", rev.Number, rev.SourcePath)
	})
	defer fetches.close()
	unterminated := map[mirrorLineEnd]bool{}
	for _, rev := range revisions {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.progress.update("activity", "Reading revision contents", "path", rev.SourcePath, "revision", rev.Number)
		entry, exists := m.entries[mirrorKey(rev)]
		var raw []byte
		if entry.Annotated {
			cached, err := m.readEntry(ctx, rev)
			if err != nil {
				return err
			}
			raw = cached.Annotation
		} else {
			var err error
			raw, err = fetches.next()
			if err != nil {
				return err
			}
		}
		annotated, labels, err := parseMirrorAnnotation(raw)
		if err != nil {
			return err
		}
		var data []byte
		if exists && entry.Blob != "" {
			if entry.Archived {
				data, err = m.annotationContent(rev, raw)
			} else {
				data, err = m.readContent(ctx, rev)
			}
			if err != nil {
				return err
			}
			m.summary.Reused++
		} else if rev.State == "dead" {
			data = annotated
			m.summary.Downloaded++
		} else {
			data, err = m.source.fetch(ctx, rev)
			if err != nil {
				return err
			}
			m.summary.Downloaded++
		}
		normalized := data
		if len(data) > 0 && data[len(data)-1] != '\n' {
			normalized = append(bytes.Clone(data), '\n')
		}
		if !bytes.Equal(normalized, annotated) {
			return diag.New("integrity", "mirror.annotate", "annotation differs from exact revision bytes", nil, "path_hex", hexBytes(rev.Path), "revision", rev.Number)
		}
		if rev.State != "dead" && len(data) > 0 && data[len(data)-1] != '\n' {
			unterminated[mirrorLineEnd{labels[len(labels)-1], digest(mirrorLastLine(data))}] = true
		}
		if !entry.Annotated {
			if err = m.saveAnnotatedEntry(rev, data, raw, true); err != nil {
				return err
			}
		}
		m.progress.update("downloaded", m.summary.Downloaded, "reused", m.summary.Reused)
	}
	// Annotation synthesizes a final newline. A live version of the same line
	// supplies that missing bit even when the surrounding contents differ. The
	// second pass reloads dead bodies individually instead of retaining history.
	if len(unterminated) == 0 {
		return nil
	}
	for _, dead := range revisions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dead.State != "dead" {
			continue
		}
		entry, err := m.readEntry(ctx, dead)
		if err != nil {
			return err
		}
		_, origins, err := parseMirrorAnnotation(entry.Annotation)
		if err != nil {
			return err
		}
		data, err := m.readContent(ctx, dead)
		if err != nil {
			return err
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			continue
		}
		last := data[:len(data)-1]
		if unterminated[mirrorLineEnd{origins[len(origins)-1], digest(mirrorLastLine(last))}] {
			if err := m.saveAnnotatedEntry(dead, last, entry.Annotation, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reread forkpoints as necessary rather than retaining every branch's base.
// The head is full text; all other revisions require just one adjacent pair.
func (m *cvsMirror) revisionDelta(ctx context.Context, revision, base *Revision) ([]byte, error) {
	if base == nil {
		return m.readContent(ctx, revision)
	}
	data, origins, err := m.readRevision(ctx, revision)
	if err != nil {
		return nil, err
	}
	previous, previousOrigins, err := m.readRevision(ctx, base)
	if err != nil {
		return nil, err
	}
	change := revision.Number
	if revision.Line == "trunk" {
		change = base.Number
	}
	return historyDelta(previous, data, previousOrigins, origins, change, revision.Line != "trunk")
}
