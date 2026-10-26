package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"cvsgitrelay/internal/diag"
)

// Proofs keep immutable source identities and exact byte checks without keeping
// another copy of every revision body and annotated source listing.
type mirrorRevisionProof struct {
	Number       string `json:"number"`
	Metadata     string `json:"metadata"`
	Blob         string `json:"blob"`
	Annotation   string `json:"annotation"`
	Unterminated bool   `json:"unterminated,omitempty"`
}

// A pending checkpoint names a fully verified staging root. Committed proofs
// omit Staging, so successful output is independent of retries and work names.
type mirrorArchive struct {
	Version   int                   `json:"version"`
	Source    []byte                `json:"source"`
	Archive   []byte                `json:"archive"`
	Metadata  string                `json:"metadata"`
	Digest    string                `json:"digest"`
	Revisions []mirrorRevisionProof `json:"revisions"`
	Staging   []byte                `json:"staging,omitempty"`
}

// The cache reader accepts the original uncompressed records for migration.
// New records use deterministic gzip streams, including the compact proofs.
func mirrorReadCompressed(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		return data, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// Blob filenames distinguish encoded cache objects from arbitrary source bytes,
// including source files that are themselves gzip streams.
func mirrorReadBlob(path string) ([]byte, error) {
	data, err := mirrorReadCompressed(path + ".gz")
	if os.IsNotExist(err) {
		return os.ReadFile(path)
	}
	return data, err
}

func mirrorWriteCompressed(path string, data []byte) error {
	return mirrorWriteStream(context.Background(), path, func(out io.Writer) error {
		writer := gzip.NewWriter(out)
		if _, err := writer.Write(data); err != nil {
			_ = writer.Close()
			return err
		}
		return writer.Close()
	})
}

func mirrorMetadataDigest(revision *Revision) string {
	data, _ := json.Marshal(mirrorMetadata(revision))
	return digest(data)
}

// Encode arbitrary CVS strings as bytes, including symbol names and owners.
// JSON's replacement of invalid UTF-8 must never hide a metadata change.
func mirrorFileDigest(file *File) string {
	type revisionIdentity struct {
		Metadata           string
		Locker, Lines, Ref []byte
		Line, Predecessor  string
		Branches           []string
	}
	identity := struct {
		Path, Source, Head, Branch, Keyword, Description []byte
		Access                                           [][]byte
		Symbols, Locks                                   [][2][]byte
		Strict, Attic                                    bool
		Revisions                                        []revisionIdentity
	}{Path: []byte(file.Path), Source: []byte(file.SourcePath), Head: []byte(file.Head), Branch: []byte(file.DefaultBranch), Keyword: []byte(file.KeywordMode), Description: file.Description, Strict: file.StrictLocks, Attic: file.InAttic}
	for _, owner := range file.Access {
		identity.Access = append(identity.Access, []byte(owner))
	}
	for index, symbols := range []map[string]string{file.Symbols, file.Locks} {
		var names []string
		for name := range symbols {
			names = append(names, name)
		}
		slices.Sort(names)
		var pairs [][2][]byte
		for _, name := range names {
			pairs = append(pairs, [2][]byte{[]byte(name), []byte(symbols[name])})
		}
		if index == 0 {
			identity.Symbols = pairs
		} else {
			identity.Locks = pairs
		}
	}
	revisions := slices.Clone(file.Revisions)
	slices.SortFunc(revisions, func(a, b *Revision) int { return compareNumber(a.Number, b.Number) })
	for _, revision := range revisions {
		identity.Revisions = append(identity.Revisions, revisionIdentity{Metadata: mirrorMetadataDigest(revision), Locker: []byte(revision.Locker), Lines: []byte(revision.Lines), Ref: []byte(revision.Ref), Line: revision.Line, Predecessor: revision.Predecessor, Branches: revision.Branches})
	}
	data, _ := json.Marshal(identity)
	return digest(data)
}

// Stream archive hashes: even a single RCS file can hold substantial history.
func mirrorFileHash(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if err == io.EOF {
			return hex.EncodeToString(hash.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

func mirrorDigestValid(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func (m *cvsMirror) archiveRoot(archive mirrorArchive) string {
	if len(archive.Staging) != 0 {
		return filepath.Join(m.output, string(archive.Staging))
	}
	return m.output
}

func (m *cvsMirror) archiveRepository(source string) *Repository {
	return &Repository{Config: Config{CVSRoot: m.archiveRoot(m.archives[source]), Module: m.source.Config.Module}, Runner: m.source.Runner}
}

// Load pending proofs over committed ones before checking backing archives:
// publication may already have replaced a file when a previous run stopped.
func (m *cvsMirror) loadArchives(ctx context.Context, inventory *Inventory) error {
	m.archives = map[string]mirrorArchive{}
	for _, directory := range []string{"archives", "pending"} {
		files, err := os.ReadDir(filepath.Join(m.state, directory))
		if err != nil {
			return err
		}
		for _, file := range files {
			if strings.HasPrefix(file.Name(), ".tmp-") {
				continue
			}
			data, err := mirrorReadCompressed(filepath.Join(m.state, directory, file.Name()))
			if err != nil {
				return err
			}
			var archive mirrorArchive
			if err := decodeJSON(data, &archive); err != nil {
				return diag.New("integrity", "mirror.archive", "invalid archive checkpoint", err)
			}
			source := string(archive.Source)
			physical := filepath.FromSlash(source)
			normal := physical + ",v"
			attic := filepath.Join(filepath.Dir(physical), "Attic", filepath.Base(physical)) + ",v"
			staging := string(archive.Staging)
			if archive.Version != 1 || validatePath(source) != nil || inventory.Files[source] == nil ||
				file.Name() != digest(archive.Source)+".json.gz" || !mirrorDigestValid(archive.Metadata) || !mirrorDigestValid(archive.Digest) ||
				(string(archive.Archive) != normal && string(archive.Archive) != attic) ||
				(directory == "archives" && staging != "") ||
				(staging != "" && (filepath.Dir(staging) != mirrorStateDir || !strings.HasPrefix(filepath.Base(staging), "work-"))) {
				return diag.New("integrity", "mirror.archive", "invalid archive identity or location", nil)
			}
			seen := map[string]bool{}
			for _, proof := range archive.Revisions {
				revision := inventory.Revisions[key(source, proof.Number)]
				if revision == nil || seen[proof.Number] || !mirrorDigestValid(proof.Blob) || !mirrorDigestValid(proof.Annotation) || proof.Metadata != mirrorMetadataDigest(revision) {
					return diag.New("integrity", "mirror.archive", "archived revision disappeared or changed in the source", nil, "path_hex", hexBytes(source), "revision", proof.Number)
				}
				seen[proof.Number] = true
			}
			// A replacement checkpoint must retain all previously durable history.
			for _, previous := range m.archives[source].Revisions {
				if !seen[previous.Number] {
					return diag.New("integrity", "mirror.archive", "pending archive omitted committed history", nil)
				}
			}
			m.archives[source] = archive
		}
	}
	for source, archive := range m.archives {
		actual, err := mirrorFileHash(ctx, filepath.Join(m.archiveRoot(archive), string(archive.Archive)))
		if err != nil || actual != archive.Digest {
			// Once publication has succeeded, a committed copy can also recover
			// a pending proof whose temporary root is no longer available.
			actual, err = mirrorFileHash(ctx, filepath.Join(m.output, string(archive.Archive)))
			if err != nil || actual != archive.Digest {
				return diag.New("integrity", "mirror.archive", "verified local archive is missing or changed", err, "path_hex", hexBytes(source))
			}
			archive.Staging = nil
			m.archives[source] = archive
		}
		for _, proof := range archive.Revisions {
			revision := inventory.Revisions[key(source, proof.Number)]
			m.entries[mirrorKey(revision)] = mirrorCachedEntry{Blob: proof.Blob, Annotated: true, AnnotationHash: proof.Annotation, Unterminated: proof.Unterminated, Archived: true}
		}
	}
	return nil
}

// CVS annotation normalizes a missing final newline. Its saved bit plus the
// content digest recovers exact live and dead bytes without a remote checkout.
func (m *cvsMirror) annotationContent(revision *Revision, raw []byte) ([]byte, error) {
	data, _, err := parseMirrorAnnotation(raw)
	if err != nil {
		return nil, err
	}
	entry := m.entries[mirrorKey(revision)]
	if entry.Unterminated {
		if len(data) == 0 || data[len(data)-1] != '\n' {
			return nil, diag.New("integrity", "mirror.archive", "invalid archived newline hint", nil)
		}
		data = data[:len(data)-1]
	}
	if digest(data) != entry.Blob {
		return nil, diag.New("integrity", "mirror.archive", "local revision content digest mismatch", nil)
	}
	return data, nil
}

func (m *cvsMirror) archivedRevision(ctx context.Context, revision *Revision) (mirrorEntry, []byte, error) {
	entry, err := m.readEntry(ctx, revision)
	if err != nil {
		return entry, nil, err
	}
	data, err := m.annotationContent(revision, entry.Annotation)
	return entry, data, err
}

// Unchanged history requires only a checked archive copy, not thousands of CVS
// subprocesses. Current checkout observations still supply permissions.
func (m *cvsMirror) reuseArchive(ctx context.Context, root string, file *File) (bool, error) {
	archive, ok := m.archives[file.SourcePath]
	if !ok || archive.Metadata != mirrorFileDigest(file) {
		return false, nil
	}
	if len(archive.Revisions) != len(file.Revisions) {
		return false, diag.New("integrity", "mirror.archive", "archive checkpoint has incomplete history", nil)
	}
	target := filepath.Join(root, mirrorArchivePath(file))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return false, err
	}
	if err := mirrorCopyFile(ctx, filepath.Join(m.archiveRoot(archive), string(archive.Archive)), target); err != nil {
		return false, err
	}
	actual, err := mirrorFileHash(ctx, target)
	if err != nil || actual != archive.Digest {
		return false, diag.New("integrity", "mirror.archive", "copied archive digest mismatch", err)
	}
	if err := os.Chmod(target, m.modes[file.SourcePath]); err != nil {
		return false, err
	}
	m.summary.Reused += len(file.Revisions)
	return true, m.discardFileDownloads(file)
}

// Make every newly created ancestor durable before replacing downloaded data
// with a staging archive as the recovery source.
func mirrorSyncParents(path, root string) error {
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		file, err := os.Open(directory)
		if err != nil {
			return err
		}
		err = file.Sync()
		_ = file.Close()
		if err != nil {
			return err
		}
		if directory == root {
			return nil
		}
	}
}

// A verified staging archive is itself a durable download cache. Checkpoint it
// before discarding expanded records, bounding scratch use to the active file.
func (m *cvsMirror) retainArchive(ctx context.Context, root string, file *File) error {
	relative, err := filepath.Rel(m.output, root)
	if err != nil {
		return err
	}
	archive := mirrorArchive{Version: 1, Source: []byte(file.SourcePath), Archive: []byte(mirrorArchivePath(file)), Metadata: mirrorFileDigest(file), Staging: []byte(relative)}
	archive.Digest, err = mirrorFileHash(ctx, filepath.Join(root, string(archive.Archive)))
	if err != nil {
		return err
	}
	for _, revision := range file.Revisions {
		entry, err := m.readEntry(ctx, revision)
		if err != nil {
			return err
		}
		data, err := m.readContent(ctx, revision)
		if err != nil {
			return err
		}
		archive.Revisions = append(archive.Revisions, mirrorRevisionProof{Number: revision.Number, Metadata: mirrorMetadataDigest(revision), Blob: entry.Blob, Annotation: digest(entry.Annotation), Unterminated: len(data) > 0 && data[len(data)-1] != '\n'})
	}
	slices.SortFunc(archive.Revisions, func(a, b mirrorRevisionProof) int { return compareNumber(a.Number, b.Number) })
	if err := mirrorSyncParents(filepath.Join(root, string(archive.Archive)), m.output); err != nil {
		return err
	}
	if err := m.writeArchiveProof("pending", archive); err != nil {
		return err
	}
	m.archives[file.SourcePath] = archive
	for _, proof := range archive.Revisions {
		revision := &Revision{SourcePath: file.SourcePath, Number: proof.Number}
		m.entries[mirrorKey(revision)] = mirrorCachedEntry{Blob: proof.Blob, Annotated: true, AnnotationHash: proof.Annotation, Unterminated: proof.Unterminated, Archived: true}
	}
	return m.discardFileDownloads(file)
}

func (m *cvsMirror) writeArchiveProof(directory string, archive mirrorArchive) error {
	data, err := json.Marshal(archive)
	if err != nil {
		return err
	}
	return mirrorWriteCompressed(filepath.Join(m.state, directory, digest(archive.Source)+".json.gz"), data)
}

// Shared blobs can disappear only when every remaining download reference has
// another durable representation. Interrupted deletions are harmless on retry.
func (m *cvsMirror) discardFileDownloads(file *File) error {
	needed := map[string]bool{}
	for _, entry := range m.entries {
		if !entry.Archived {
			needed[entry.Blob] = true
		}
	}
	for _, revision := range file.Revisions {
		if err := os.Remove(filepath.Join(m.state, "revisions", mirrorKey(revision)+".json")); err != nil && !os.IsNotExist(err) {
			return err
		}
		blob := m.entries[mirrorKey(revision)].Blob
		if blob != "" && !needed[blob] {
			for _, name := range []string{blob, blob + ".gz"} {
				if err := os.Remove(filepath.Join(m.state, "blobs", name)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	return nil
}

// Publish all compact proofs before removing pending checkpoints or staging
// roots. A crash anywhere in this sequence retains a complete recovery source.
func (m *cvsMirror) commitArchives(ctx context.Context) error {
	var sources []string
	for source := range m.archives {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	for _, source := range sources {
		archive := m.archives[source]
		actual, err := mirrorFileHash(ctx, filepath.Join(m.output, string(archive.Archive)))
		if err != nil || actual != archive.Digest {
			return diag.New("integrity", "mirror.archive", "published archive digest mismatch", err, "path_hex", hexBytes(source))
		}
		archive.Staging = nil
		if err := m.writeArchiveProof("archives", archive); err != nil {
			return err
		}
		m.archives[source] = archive
	}
	files, err := os.ReadDir(filepath.Join(m.state, "pending"))
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := os.Remove(filepath.Join(m.state, "pending", file.Name())); err != nil {
			return err
		}
	}
	// Persist pending deletion before cleanup can remove its backing roots.
	return mirrorSyncParents(filepath.Join(m.state, "pending", "checkpoint"), m.output)
}
