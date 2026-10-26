package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
)

const mirrorStateDir = ".cvsgitrelay-mirror"

// MirrorSummary separates remote content downloads from locally reused history.
type MirrorSummary struct {
	Operation    string `json:"operation"`
	CVSRoot      string `json:"cvsroot"`
	Module       string `json:"module"`
	Files        int    `json:"files"`
	Revisions    int    `json:"revisions"`
	Downloaded   int    `json:"downloaded"`
	Reused       int    `json:"reused"`
	UpdatedFiles int    `json:"updated_files"`
}

// Only logical source selection belongs in persistent identity. Transport,
// output path, run time and polling history cannot affect mirror contents.
type mirrorIdentity struct {
	Version int             `json:"version"`
	Module  string          `json:"module"`
	Layout  []ModuleMapping `json:"layout"`
}

// Entries describe immutable revision metadata and content, not discovery
// history. Mutable file symbols and default branches live in generated RCS.
type mirrorEntry struct {
	Revision   mirrorRevision `json:"revision"`
	Blob       string         `json:"blob,omitempty"`
	Annotation []byte         `json:"annotation,omitempty"`
	Annotated  bool           `json:"annotated,omitempty"`
}

// Byte slices use JSON base64 so supported non-UTF-8 paths and authors survive
// cache round trips without JSON's replacement of invalid string bytes.
type mirrorRevision struct {
	Path       []byte
	SourcePath []byte
	Number     string
	Author     []byte
	State      string
	Time       int64
	Log        []byte
	CommitID   []byte
}

type cvsMirror struct {
	source              *Repository
	progress            *mirrorProgress
	output              string
	state               string
	entries             map[string]mirrorEntry
	summary             MirrorSummary
	modes               map[string]fs.FileMode
	directories         []string
	requiredDirectories map[string]bool
	administration      map[string][]byte
	directoryModes      map[string]fs.FileMode
	modeKnown           map[string]bool
}

func mirrorMetadata(revision *Revision) mirrorRevision {
	return mirrorRevision{Path: []byte(revision.Path), SourcePath: []byte(revision.SourcePath), Number: revision.Number,
		Author: []byte(revision.Author), State: revision.State, Time: revision.Time,
		Log: revision.Log, CommitID: []byte(revision.CommitID)}
}

func mirrorKey(revision *Revision) string {
	return digest([]byte(key(revision.SourcePath, revision.Number)))
}

// Mirror captures all CVS-visible history and reuses durable revision downloads
// on subsequent runs. Archive publication is atomic per file; retrying completes
// an interrupted publication. Consumers should run after the writer finishes.
func Mirror(ctx context.Context, runner *process.Runner, root, module, output string, requireConsecutive bool, options Options) (summary MirrorSummary, err error) {
	if output == "" || options.Connections < 1 {
		return summary, diag.New("config", "mirror", "output directory and positive connection limit are required", nil)
	}
	if err := validateConfig(Config{Version: 1, SourceID: strings.Repeat("0", 32), CVSRoot: root, Module: module, KeywordMode: "kb", FileMode: "100644", Scope: "all", RefFormat: "reftable"}); err != nil {
		return summary, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return summary, err
	}
	// A local source and its generated output must never contain one another;
	// otherwise recursive discovery or publication could ingest its own output.
	if strings.HasPrefix(root, "/") || strings.HasPrefix(root, ":local:") || strings.HasPrefix(root, ":fork:") {
		sourcePath, _ := sourceDirectory(root)
		sourcePath, err = filepath.EvalSymlinks(sourcePath)
		if err != nil {
			return summary, err
		}
		parent, resolveErr := filepath.EvalSymlinks(filepath.Dir(output))
		if resolveErr != nil {
			return summary, resolveErr
		}
		resolved := filepath.Join(parent, filepath.Base(output))
		if resolved == sourcePath || strings.HasPrefix(resolved, sourcePath+"/") || strings.HasPrefix(sourcePath, resolved+"/") {
			return summary, diag.New("config", "mirror.create", "local CVS source and mirror output must not overlap", nil)
		}
	}
	if err = os.Mkdir(output, 0700); err != nil && !os.IsExist(err) {
		return summary, diag.New("io", "mirror.create", "output requires an existing parent directory", err)
	}
	defer func() {
		var pathError *os.PathError
		if err != nil && errors.As(err, &pathError) && diag.Code(err) == "internal" {
			err = diag.New("io", "mirror.write", "cannot update local CVSROOT", err)
		}
	}()
	// Reject symlinks before creating any state or replacing generated files.
	// An existing directory must be managed, empty, or an earlier generated copy.
	if err = filepath.WalkDir(output, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return diag.New("config", "mirror.open", "mirror directories must not contain symlinks", nil)
		}
		return nil
	}); err != nil {
		return summary, err
	}
	m := &cvsMirror{output: output, state: filepath.Join(output, mirrorStateDir), entries: map[string]mirrorEntry{},
		source: &Repository{Config: Config{CVSRoot: root, Module: module}, Runner: runner, Submodules: "include", mirroring: true, RequireConsecutiveRevisions: requireConsecutive}}
	m.modes = map[string]fs.FileMode{}
	m.directoryModes = map[string]fs.FileMode{}
	m.modeKnown = map[string]bool{}
	m.requiredDirectories = map[string]bool{}
	m.summary = MirrorSummary{Operation: "mirror", CVSRoot: output, Module: module}
	identityPath := filepath.Join(m.state, "source.json")
	identityData, readErr := os.ReadFile(identityPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return summary, readErr
	}
	legacy := false
	if os.IsNotExist(readErr) {
		entries, err := os.ReadDir(output)
		if err != nil {
			return summary, err
		}
		for _, entry := range entries {
			if entry.Name() != mirrorStateDir {
				legacy = true
			}
		}
		if legacy {
			archive, err := os.ReadFile(filepath.Join(output, "CVSROOT", "modules,v"))
			if err != nil || !bytes.Contains(archive, []byte("author cvsgitrelay; state Exp;")) {
				return summary, diag.New("config", "mirror.open", "existing output is not a cvsgitrelay CVS mirror", nil)
			}
		}
	}
	if err = os.MkdirAll(m.state, 0700); err != nil {
		return summary, err
	}
	lock, err := os.OpenFile(filepath.Join(m.state, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return summary, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return summary, diag.New("locked", "mirror.lock", "another process is updating this CVS mirror", err)
	}
	defer func() {
		if err != nil {
			err = diag.New(diag.Code(err), "mirror.retain", "mirror failed; local output and completed downloads have been retained; retry with the same output directory", err, "output", output, "cache", m.state)
		}
	}()
	// Failed attempts retain their generated archives and checkout observations.
	// A retry must not discard them before it has published a validated mirror.
	m.progress = newMirrorProgress(ctx, runner.Log, options.ProgressInterval)
	defer m.progress.close()
	m.progress.stage("Scanning source CVS history", "module", module)
	before, err := m.source.mirrorScan(ctx, m.progress)
	if err != nil {
		return summary, err
	}
	identity := mirrorIdentity{Version: 2, Module: module, Layout: before.Layout}
	if readErr == nil {
		var previous mirrorIdentity
		err = decodeJSON(identityData, &previous)
		if previous.Version == 1 {
			previous.Version = 2
		}
		if err != nil || !reflect.DeepEqual(previous, identity) {
			return summary, diag.New("config", "mirror.identity", "module or physical source layout differs from this mirror", err)
		}
	}
	for _, mapping := range before.Layout {
		if mapping.Source == mirrorStateDir || strings.HasPrefix(mapping.Source, mirrorStateDir+"/") {
			return summary, diag.New("unsupported", "mirror", "administrative directories cannot be mirrored as source modules", nil)
		}
	}
	for _, name := range []string{"revisions", "blobs"} {
		if err = os.MkdirAll(filepath.Join(m.state, name), 0700); err != nil {
			return summary, err
		}
	}
	m.progress.stage("Loading retained mirror downloads", "revisions", len(before.Revisions))
	if err = m.loadEntries(before); err != nil {
		return summary, err
	}
	runner.Log.Info("Retained mirror downloads loaded", "cached_revisions", len(m.entries))
	if legacy {
		// Bootstrap the durable cache from the earlier local copy. No old live
		// revision needs to be downloaded again from the remote CVS server.
		m.progress.stage("Reading existing local mirror history")
		local := &Repository{Config: Config{CVSRoot: output, Module: module}, Runner: runner, Submodules: "include", mirroring: true}
		previous, err := local.scan(ctx)
		if err != nil {
			return summary, err
		}
		if !sameMappings(previous.Layout, before.Layout) {
			return summary, diag.New("config", "mirror.adopt", "existing copy has a different module layout", nil)
		}
		for _, revision := range previous.Ordered {
			current := before.Revisions[key(revision.Path, revision.Number)]
			if current == nil || !reflect.DeepEqual(mirrorMetadata(revision), mirrorMetadata(current)) {
				return summary, diag.New("integrity", "mirror.adopt", "existing revision disappeared or changed in the source", nil, "path_hex", hexBytes(revision.Path), "revision", revision.Number)
			}
		}
		for _, revision := range previous.Ordered {
			if _, ok := m.entries[mirrorKey(revision)]; ok {
				continue
			}
			var data []byte
			if revision.State != "dead" {
				data, err = local.fetch(ctx, revision)
				if err != nil {
					return summary, err
				}
			}
			if err = m.saveEntry(revision, data); err != nil {
				return summary, err
			}
		}
	}
	encodedIdentity, _ := json.Marshal(identity)
	if err = mirrorWrite(identityPath, encodedIdentity); err != nil {
		return summary, err
	}
	m.progress.stage("Reading CVS module definitions")
	_, definitions, err := m.source.moduleLayout(ctx)
	if err != nil {
		return summary, err
	}
	if err = m.observeTree(ctx, before); err != nil {
		return summary, err
	}
	staging, err := os.MkdirTemp(m.state, "work-")
	if err != nil {
		return summary, err
	}
	if err = mirrorAdmin(staging, module, definitions); err != nil {
		return summary, err
	}
	for _, directory := range m.directories {
		if err = os.MkdirAll(filepath.Join(staging, filepath.FromSlash(directory)), 0700); err != nil {
			return summary, err
		}
	}
	paths := make([]string, 0, len(before.Files))
	for path := range before.Files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	runner.Log.Info("Mirror inventory ready", "files", len(paths), "revisions", len(before.Revisions))
	m.progress.stage("Acquiring mirror revisions", "files", len(paths), "revisions", len(before.Revisions), "files_completed", 0, "downloaded", 0, "reused", 0)
	for i, path := range paths {
		m.progress.update("path", path)
		if err = m.writeFile(ctx, staging, before.Files[path], options.Connections); err != nil {
			return summary, err
		}
		m.progress.update("files_completed", i+1)
	}
	// Matching scans bound source movement; they do not assert a server-wide
	// atomic snapshot. Cached immutable revisions are checked before any publish.
	m.progress.stage("Checking source CVS history again", "module", module)
	after, err := m.source.mirrorScan(ctx, m.progress)
	if err != nil {
		return summary, err
	}
	if !reflect.DeepEqual(before, after) {
		return summary, diag.New("source", "mirror.compare", "CVS history changed during mirroring; rerun to reuse completed downloads", nil)
	}
	local := &Repository{Config: Config{CVSRoot: staging, Module: module}, Runner: runner, Submodules: "include", mirroring: true}
	m.progress.stage("Checking generated CVS history", "module", module)
	copied, err := local.mirrorScan(ctx, m.progress)
	if err != nil {
		return summary, err
	}
	if !reflect.DeepEqual(before, copied) {
		return summary, diag.New("integrity", "mirror.compare", "generated CVS history differs from source metadata", nil)
	}
	if err = ctx.Err(); err != nil {
		return summary, err
	}
	m.progress.stage("Publishing local CVS mirror", "downloaded", m.summary.Downloaded, "reused", m.summary.Reused)
	if err = m.preserveAdministration(staging, before); err != nil {
		return summary, err
	}
	if err = m.publish(ctx, staging); err != nil {
		return summary, err
	}
	m.progress.stage("Normalizing local CVS mirror")
	if err = m.canonicalize(before); err != nil {
		return summary, err
	}
	m.progress.stage("Removing temporary work after successful publication")
	if err = m.cleanupWork(); err != nil {
		return summary, err
	}
	m.summary.Files, m.summary.Revisions = len(before.Files), len(before.Revisions)
	runner.Log.Info("Mirror complete", "files", m.summary.Files, "revisions", m.summary.Revisions, "downloaded", m.summary.Downloaded, "reused", m.summary.Reused)
	return m.summary, nil
}

// Discard derived work only after validation, publication and normalization
// succeed. Until then even work retained by an earlier attempt stays available.
func (m *cvsMirror) cleanupWork() error {
	entries, err := os.ReadDir(m.state)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "work-") {
			if err := os.RemoveAll(filepath.Join(m.state, entry.Name())); err != nil {
				return err
			}
		}
	}
	// Removing work changes its parent's mtime; successful mirrors still converge.
	return os.Chtimes(m.state, time.Unix(0, 0), time.Unix(0, 0))
}

// Durable, atomic replacement keeps cache records and archive files complete
// across interruptions. Equal bytes are left alone, including on no-op polls.
func mirrorWrite(path string, data []byte) error {
	old, err := os.ReadFile(path)
	if err == nil && bytes.Equal(old, data) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), path)
	}
	if err == nil {
		dir, openErr := os.Open(filepath.Dir(path))
		if openErr != nil {
			return openErr
		}
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}

func (m *cvsMirror) saveEntry(revision *Revision, data []byte) error {
	return m.saveAnnotatedEntry(revision, data, nil, false)
}

func (m *cvsMirror) saveAnnotatedEntry(revision *Revision, data, annotation []byte, annotated bool) error {
	entry := mirrorEntry{Revision: mirrorMetadata(revision), Annotation: annotation, Annotated: annotated}
	if annotated || revision.State != "dead" {
		entry.Blob = digest(data)
		if err := mirrorWrite(filepath.Join(m.state, "blobs", entry.Blob), data); err != nil {
			return err
		}
	}
	encoded, _ := json.Marshal(entry)
	id := mirrorKey(revision)
	if err := mirrorWrite(filepath.Join(m.state, "revisions", id+".json"), encoded); err != nil {
		return err
	}
	m.entries[id] = entry
	return nil
}

func (m *cvsMirror) loadEntries(inventory *Inventory) error {
	files, err := os.ReadDir(filepath.Join(m.state, "revisions"))
	if err != nil {
		return err
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".tmp-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.state, "revisions", file.Name()))
		if err != nil {
			return err
		}
		var entry mirrorEntry
		if err := decodeJSON(data, &entry); err != nil {
			return diag.New("integrity", "mirror.cache", "invalid cached revision metadata", err)
		}
		current := inventory.Revisions[key(string(entry.Revision.SourcePath), entry.Revision.Number)]
		entry.Revision.Path = entry.Revision.SourcePath
		if current == nil || !reflect.DeepEqual(entry.Revision, mirrorMetadata(current)) || file.Name() != mirrorKey(current)+".json" {
			return diag.New("integrity", "mirror.cache", "cached revision disappeared or changed in the source", nil, "path_hex", hexBytes(string(entry.Revision.Path)), "revision", entry.Revision.Number)
		}
		if (entry.Blob == "" && (current.State != "dead" || entry.Annotated)) || (entry.Blob != "" && (len(entry.Blob) != 64 || strings.Trim(entry.Blob, "0123456789abcdef") != "")) {
			return diag.New("integrity", "mirror.cache", "invalid cached content digest", nil)
		}
		m.entries[mirrorKey(current)] = entry
	}
	return nil
}

func (m *cvsMirror) contents(ctx context.Context, revisions []*Revision, connections int) (map[string][]byte, map[string][]string, error) {
	contents := map[string][]byte{}
	origins := map[string][]string{}
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
	for _, rev := range revisions {
		m.progress.update("activity", "Reading revision contents", "path", rev.SourcePath, "revision", rev.Number)
		entry, exists := m.entries[mirrorKey(rev)]
		raw := entry.Annotation
		if !exists || !entry.Annotated {
			var err error
			raw, err = fetches.next()
			if err != nil {
				return nil, nil, err
			}
		}
		annotated, labels, err := parseMirrorAnnotation(raw)
		if err != nil {
			return nil, nil, err
		}
		var data []byte
		if exists && entry.Blob != "" {
			data, err = os.ReadFile(filepath.Join(m.state, "blobs", entry.Blob))
			if err != nil {
				return nil, nil, err
			}
			if digest(data) != entry.Blob {
				return nil, nil, diag.New("integrity", "mirror.cache", "cached content digest mismatch", nil)
			}
			m.summary.Reused++
		} else if rev.State == "dead" {
			data = annotated
			m.summary.Downloaded++
		} else {
			data, err = m.source.fetch(ctx, rev)
			if err != nil {
				return nil, nil, err
			}
			m.summary.Downloaded++
		}
		normalized := data
		if len(data) > 0 && data[len(data)-1] != '\n' {
			normalized = append(bytes.Clone(data), '\n')
		}
		if !bytes.Equal(normalized, annotated) {
			return nil, nil, diag.New("integrity", "mirror.annotate", "annotation differs from exact revision bytes", nil, "path_hex", hexBytes(rev.Path), "revision", rev.Number)
		}
		if !entry.Annotated {
			if err = m.saveAnnotatedEntry(rev, data, raw, true); err != nil {
				return nil, nil, err
			}
		}
		m.progress.update("downloaded", m.summary.Downloaded, "reused", m.summary.Reused)
		contents[rev.Number], origins[rev.Number] = data, labels
	}
	// Annotation synthesizes a final newline. An unchanged unterminated line
	// in any live version with the same origin supplies the missing boundary bit,
	// even when other lines changed during the deletion (for example keywords).
	for _, dead := range revisions {
		if dead.State != "dead" {
			continue
		}
		data := contents[dead.Number]
		lines := fileLines(data)
		if len(lines) == 0 || len(data) == 0 || data[len(data)-1] != '\n' {
			continue
		}
		last := len(lines) - 1
		for _, live := range revisions {
			if live.State == "dead" {
				continue
			}
			liveLines := fileLines(contents[live.Number])
			matched := false
			for i, line := range liveLines {
				if len(line) > 0 && line[len(line)-1] != '\n' && origins[live.Number][i] == origins[dead.Number][last] && bytes.Equal(line, bytes.TrimSuffix(lines[last], []byte{'\n'})) {
					matched = true
					break
				}
			}
			if matched {
				contents[dead.Number] = data[:len(data)-1]
				entry := m.entries[mirrorKey(dead)]
				if err := m.saveAnnotatedEntry(dead, contents[dead.Number], entry.Annotation, true); err != nil {
					return nil, nil, err
				}
				break
			}
		}
	}
	return contents, origins, nil
}

// Minimal administration is fixed across CVS versions and machines. In
// particular, CVS init would embed the invoking user and current time in RCS.
func mirrorAdmin(root, module string, definitions map[string][]string) error {
	if err := os.MkdirAll(filepath.Join(root, "CVSROOT"), 0700); err != nil {
		return err
	}
	selected := map[string][]string{}
	var visit func(string)
	visit = func(name string) {
		args, ok := definitions[name]
		if _, seen := selected[name]; !ok || seen {
			return
		}
		selected[name] = slices.Clone(args)
		if args[0] == "-a" {
			slices.Sort(selected[name][1:])
			for _, child := range args[1:] {
				visit(child)
			}
		}
	}
	visit(module)
	var names []string
	for name := range selected {
		names = append(names, name)
	}
	slices.Sort(names)
	var modules strings.Builder
	for _, name := range names {
		fmt.Fprintf(&modules, "%s %s\n", name, strings.Join(selected[name], " "))
	}
	for _, file := range []struct{ name, data string }{{"modules", modules.String()}, {"config", "LogHistory=\n"}, {"val-tags", ""}} {
		if err := mirrorWrite(filepath.Join(root, "CVSROOT", file.name), []byte(file.data)); err != nil {
			return err
		}
	}
	return nil
}

// Publish only fully validated files. A failed multi-file publication can be
// completed on retry using the durable cache, without new remote downloads.
func (m *cvsMirror) publish(ctx context.Context, staging string) error {
	wanted := map[string]bool{}
	if err := filepath.WalkDir(staging, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(staging, path)
		if err != nil {
			return err
		}
		wanted[relative] = true
		if entry.IsDir() {
			m.requiredDirectories[filepath.Join(m.output, relative)] = true
		}
		target := filepath.Join(m.output, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		old, readErr := os.ReadFile(target)
		if readErr == nil && bytes.Equal(old, data) {
			expected, err := entry.Info()
			if err != nil {
				return err
			}
			actual, err := os.Stat(target)
			if err != nil {
				return err
			}
			if expected.Mode().Perm() != actual.Mode().Perm() {
				if err = os.Chmod(target, expected.Mode().Perm()); err != nil {
					return err
				}
				if strings.HasSuffix(relative, ",v") {
					m.summary.UpdatedFiles++
				}
			}
			return nil
		}
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		if err := mirrorWrite(target, data); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err = os.Chmod(target, info.Mode().Perm()); err != nil {
			return err
		}
		if strings.HasSuffix(relative, ",v") {
			m.summary.UpdatedFiles++
		}
		return nil
	}); err != nil {
		return err
	}
	// Old Attic locations and legacy initialization files must not make fresh
	// and iterative copies differ. The managed tree belongs to this generator.
	if err := filepath.WalkDir(m.output, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == m.state {
			return filepath.SkipDir
		}
		relative, err := filepath.Rel(m.output, path)
		if err != nil {
			return err
		}
		if !entry.IsDir() && !wanted[relative] {
			return os.Remove(path)
		}
		return nil
	}); err != nil {
		return err
	}
	// Administrative working files can disappear while their dead archive stays.
	// Remove obsolete preserved heads without touching the durable revision cache.
	admin := filepath.Join(m.state, "administration")
	if _, err := os.Stat(admin); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(admin, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(m.output, path)
		if err != nil {
			return err
		}
		if !entry.IsDir() && !wanted[relative] {
			return os.Remove(path)
		}
		return nil
	})
}

// Canonical output includes only current cache entries, fixed modes and epoch
// mtimes. No wall-clock values or abandoned temporary work survive success.
func (m *cvsMirror) canonicalize(inventory *Inventory) error {
	usedBlobs := map[string]bool{}
	for _, entry := range m.entries {
		usedBlobs[entry.Blob] = true
	}
	if err := filepath.WalkDir(m.state, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filepath.Dir(path) == m.state && entry.IsDir() && strings.HasPrefix(entry.Name(), "work-") {
			return filepath.SkipDir
		}
		if strings.HasPrefix(entry.Name(), ".tmp-") || (filepath.Dir(path) == filepath.Join(m.state, "blobs") && !usedBlobs[entry.Name()]) {
			return os.Remove(path)
		}
		return nil
	}); err != nil {
		return err
	}
	keep := map[string]bool{m.output: true, m.state: true, filepath.Join(m.output, "CVSROOT"): true,
		filepath.Join(m.state, "revisions"): true, filepath.Join(m.state, "blobs"): true}
	for _, mapping := range inventory.Layout {
		for dir := filepath.Join(m.output, filepath.FromSlash(mapping.Source)); dir != m.output; dir = filepath.Dir(dir) {
			keep[dir] = true
		}
	}
	for directory := range m.requiredDirectories {
		keep[directory] = true
	}
	archiveModes := map[string]fs.FileMode{}
	for _, file := range inventory.Files {
		archiveModes[filepath.Join(m.output, mirrorArchivePath(file))] = m.modes[file.SourcePath]
	}
	var paths []string
	if err := filepath.WalkDir(m.output, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && filepath.Dir(path) == m.state && entry.IsDir() && strings.HasPrefix(entry.Name(), "work-") {
			return filepath.SkipDir
		}
		if walkErr == nil {
			paths = append(paths, path)
		}
		return walkErr
	}); err != nil {
		return err
	}
	for i := len(paths) - 1; i >= 0; i-- {
		path := paths[i]
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0600)
		if observed, ok := archiveModes[path]; ok {
			mode = observed
		}
		if info.IsDir() {
			children, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			if len(children) == 0 && !keep[path] {
				if err := os.Remove(path); err != nil {
					return err
				}
				continue
			}
			mode = 0700
			relative, _ := filepath.Rel(m.output, path)
			if observed, ok := m.directoryModes[filepath.ToSlash(relative)]; ok {
				mode = observed
			}
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		if err := os.Chtimes(path, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return err
		}
	}
	return nil
}
