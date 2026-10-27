package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

// durableFile creates initialization evidence once, then synchronizes the
// containing directory. Recovery intent itself lives under a Git ref.
func durableFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cvsgitrelay-init-")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Link installs a complete, fsynced file without replacing an existing
	// configuration or witness. Both paths are on the same filesystem.
	if err := os.Link(temporary, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (r *Repository) publicationMarker() string {
	return filepath.Join(r.Path, "cvsgitrelay.initialized")
}
func (r *Repository) markerBytes() []byte {
	return []byte("cvsgitrelay-publication-witness-v2\npolicy=" + r.Config.Policy.Digest() + "\n")
}
func (r *Repository) hasPublicationMarker() (bool, error) {
	data, err := os.ReadFile(r.publicationMarker())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(data, r.markerBytes()) {
		return true, diag.New("integrity", "recovery.witness", "invalid initial-publication evidence", nil)
	}
	return true, nil
}
func (r *Repository) ensurePublicationMarker() error {
	exists, err := r.hasPublicationMarker()
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return durableFile(r.publicationMarker(), r.markerBytes())
}

func refValue(oid string) string {
	if oid == "" {
		return zeroOID
	}
	return oid
}

// CAS always uses no-deref, including deletions and ref creation. Deliberate
// graph rewrites do not pass through branch fast-forward checks.
func (r *Repository) cas(ctx context.Context, ref, old, next string) error {
	line := "update " + ref + " " + refValue(next) + " " + refValue(old) + "\n"
	if next == "" {
		line = "delete " + ref + " " + refValue(old) + "\n"
	}
	_, err := r.git(ctx, []byte("option no-deref\n"+line), nil, "update-ref", "--stdin")
	if err != nil {
		return diag.New("conflict", "refs.cas", "ref compare-and-swap failed", err, "ref", ref)
	}
	return nil
}

// Writer-side verification closes the interval between reading recovery state
// and changing a ref. Only one ref is mutated; readers still resolve one state
// OID rather than assuming multi-ref snapshot isolation.
func (r *Repository) evidenceCAS(ctx context.Context, ref, old, next, state, journal string, checks map[string]string) error {
	var commands strings.Builder
	commands.WriteString("start\noption no-deref\n")
	if ref != stateRef {
		fmt.Fprintf(&commands, "verify %s %s\n", stateRef, refValue(state))
	}
	if ref != journalRef {
		fmt.Fprintf(&commands, "verify %s %s\n", journalRef, journal)
	}
	for _, name := range canonical.SortedKeys(checks) {
		if name != ref {
			fmt.Fprintf(&commands, "verify %s %s\n", name, refValue(checks[name]))
		}
	}
	if next == "" {
		fmt.Fprintf(&commands, "delete %s %s\n", ref, refValue(old))
	} else {
		fmt.Fprintf(&commands, "update %s %s %s\n", ref, next, refValue(old))
	}
	commands.WriteString("prepare\ncommit\n")
	if _, err := r.git(ctx, []byte(commands.String()), nil, "update-ref", "--stdin"); err != nil {
		return diag.New("conflict", "refs.evidence-cas", "publication evidence or expected ref changed", err, "ref", ref)
	}
	return nil
}

func projectionChecks(i publicationIntent, next bool) map[string]string {
	checks := map[string]string{}
	for ref := range i.Expected {
		checks[ref] = ""
	}
	for ref := range i.Desired {
		checks[ref] = ""
	}
	targets := i.Expected
	if next {
		targets = i.Desired
	}
	for ref, oid := range targets {
		checks[ref] = oid
	}
	return checks
}

// Object writes use the byte encoder's payload, never commit-tree's environment.
// Re-read every object to detect pre-existing damaged objects as well as writes.
// Objects are processed in sorted chunks: missing ones are written per kind in
// one batch, then the whole chunk is read back and compared before its
// failpoints run, so an interruption still leaves a verified prefix.
func (r *Repository) storeObjects(ctx context.Context, objects canonical.Objects, opts *Options) (int, error) {
	written := 0
	keys := canonical.SortedKeys(objects)
	for _, oid := range keys {
		if o := objects[oid]; canonical.OID(o.Kind, o.Data) != oid {
			return written, diag.New("integrity", "objects.write", "builder emitted incorrect object ID", nil)
		}
	}
	if err := r.removeObjectStaging(); err != nil {
		return written, err
	}
	existing, err := r.probeObjects(ctx, keys)
	if err != nil {
		return 0, err
	}
	emptyTreePath := filepath.Join(r.Path, "objects", emptyTreeOID[:2], emptyTreeOID[2:])
	// hash-object prints one ID line per written object.
	rows := r.batchRows(writeBatchObjects, 41)
	for start := 0; start < len(keys); {
		end, size := start, 0
		for end < len(keys) && end-start < rows && (end == start || size+len(objects[keys[end]].Data) <= writeBatchBytes) {
			size += len(objects[keys[end]].Data)
			end++
		}
		chunk := keys[start:end]
		missing := map[string][]string{}
		var emptyTreeBefore error
		forceEmptyTree := false
		for _, oid := range chunk {
			// Git can synthesize the empty tree for cat-file without storing it.
			// Force its write so GC, transfer and fsck have an actual object.
			if oid == emptyTreeOID {
				forceEmptyTree = true
				_, emptyTreeBefore = os.Stat(emptyTreePath)
			} else if _, ok := existing[oid]; ok {
				continue
			}
			missing[objects[oid].Kind] = append(missing[objects[oid].Kind], oid)
		}
		for _, kind := range canonical.SortedKeys(missing) {
			if err := r.writeObjects(ctx, kind, missing[kind], objects); err != nil {
				return written, err
			}
			for _, oid := range missing[kind] {
				if oid != emptyTreeOID {
					written++
					r.objectWrites++
				}
			}
		}
		if forceEmptyTree {
			if _, after := os.Stat(emptyTreePath); os.IsNotExist(emptyTreeBefore) && after == nil {
				written++
				r.objectWrites++
			}
		}
		err := r.readObjects(ctx, chunk, func(oid, kind string, data []byte) error {
			if o := objects[oid]; kind != o.Kind || !bytes.Equal(data, o.Data) {
				return diag.New("integrity", "objects.read", "stored object bytes differ from canonical payload", nil)
			}
			return nil
		})
		if err != nil {
			return written, err
		}
		if opts != nil {
			for _, oid := range chunk {
				stage := map[string]string{"blob": "blob-written", "tree": "tree-written", "commit": "commit-written"}[objects[oid].Kind]
				if err := opts.hit(stage); err != nil {
					return written, err
				}
			}
		}
		start = end
	}
	return written, nil
}

// compareObjects checks that every expected object is stored with exactly the
// expected kind and payload, using batched reads.
func (r *Repository) compareObjects(ctx context.Context, objects canonical.Objects) error {
	return r.readObjects(ctx, canonical.SortedKeys(objects), func(oid, kind string, data []byte) error {
		if o := objects[oid]; kind != o.Kind || !bytes.Equal(data, o.Data) {
			return fmt.Errorf("stored object bytes differ")
		}
		return nil
	})
}

func emptyManifest(p canonical.Policy) canonical.Manifest {
	return canonical.Manifest{Source: p.Source, PolicyDigest: p.Digest(), Verify: p.Verify, Revisions: map[string]canonical.RevisionEntry{}, Lines: map[string]string{}, Refs: map[string]string{}}
}

// readState validates the deterministic state/anchor envelope and coverage using
// real commit ancestry. It needs neither source access nor a recovery journal.
func (r *Repository) readState(ctx context.Context, oid string) (canonical.Manifest, error) {
	if oid == "" {
		return emptyManifest(r.Config.Policy), nil
	}
	fail := func(err error) (canonical.Manifest, error) {
		return canonical.Manifest{}, diag.New("integrity", "state.read", "invalid canonical generation", err, "state", oid)
	}
	data, err := r.git(ctx, nil, nil, "show", oid+":manifest.v2")
	if err != nil {
		return fail(err)
	}
	m, err := canonical.ParseManifest(data)
	if err != nil {
		return fail(err)
	}
	if m.Source != r.Config.SourceID || m.PolicyDigest != r.Config.Policy.Digest() || m.Verify != r.Config.Policy.Verify {
		return fail(fmt.Errorf("canonical policy mismatch; use a new destination"))
	}
	o := canonical.Objects{}
	blob := o.Put("blob", data)
	tree, err := o.Tree(map[string]string{"manifest.v2": blob})
	if err != nil {
		return fail(err)
	}
	var tips []string
	for id, tip := range m.Lines {
		ref, e := canonical.EncodedRef("refs/cvsgitrelay/lines/", id)
		if e != nil || m.Refs[ref] != tip {
			return fail(fmt.Errorf("line projection missing"))
		}
		tips = append(tips, tip)
	}
	for ref, tip := range m.Refs {
		if strings.HasPrefix(ref, "refs/tags/cvs/") {
			tips = append(tips, tip)
		} else if !slices.Contains(tips, tip) {
			found := false
			for _, lineTip := range m.Lines {
				if lineTip == tip {
					found = true
				}
			}
			if !found {
				return fail(fmt.Errorf("projection is not a line tip"))
			}
		}
	}
	anchor := o.Anchor(tips)
	var parents []string
	if anchor != "" {
		parents = []string{anchor}
	}
	expected := o.Synthetic(tree, parents, "cvsgitrelay-state-v2\n")
	if expected != oid {
		return fail(fmt.Errorf("state/manifest envelope mismatch"))
	}
	if err := r.compareObjects(ctx, o); err != nil {
		return fail(fmt.Errorf("canonical envelope bytes differ: %w", err))
	}
	ancestry, err := r.git(ctx, nil, nil, "rev-list", oid)
	if err != nil {
		return fail(err)
	}
	reachable := map[string]bool{}
	for _, id := range strings.Fields(string(ancestry)) {
		reachable[id] = true
	}
	// Events are unique, so each one maps back to exactly one manifest key.
	keys := map[string]string{}
	var events []string
	for _, key := range canonical.SortedKeys(m.Revisions) {
		event := m.Revisions[key].Event
		if !reachable[event] || keys[event] != "" {
			return fail(fmt.Errorf("duplicate or unreachable revision event"))
		}
		keys[event] = key
		events = append(events, event)
	}
	err = r.readObjects(ctx, events, func(event, kind string, data []byte) error {
		if kind != "commit" {
			return fmt.Errorf("revision event is not a commit")
		}
		head, message, ok := bytes.Cut(data, []byte("\n\n"))
		if !ok {
			return fmt.Errorf("event message missing")
		}
		rev, e := r.Config.Policy.ParseRevisionMessage(message)
		if e != nil || rev.Key.ID() != keys[event] {
			return fmt.Errorf("event key mismatch")
		}
		lines := strings.Split(string(head), "\n")
		if len(lines) != 4 || !strings.HasPrefix(lines[0], "tree ") || !strings.HasPrefix(lines[1], "parent ") {
			return fmt.Errorf("invalid event ancestry")
		}
		expected := canonical.CommitBytes(strings.TrimPrefix(lines[0], "tree "), []string{strings.TrimPrefix(lines[1], "parent ")}, r.Config.Policy.EventAuthor(rev.Author), rev.Time, message)
		if !bytes.Equal(data, expected) {
			return fmt.Errorf("noncanonical event header")
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	for _, tip := range m.Refs {
		if !reachable[tip] {
			return fail(fmt.Errorf("unreachable projection"))
		}
	}
	if _, err := r.git(ctx, nil, nil, "fsck", "--strict", "--no-dangling", oid); err != nil {
		return fail(err)
	}
	return m, nil
}

func validateProjections(refs map[string]string, state string, expected map[string]string, allowJournal bool) error {
	if refs[stateRef] != state {
		return diag.New("conflict", "refs.validate", "authoritative state changed unexpectedly", nil)
	}
	for ref, oid := range expected {
		if refs[ref] != oid {
			return diag.New("conflict", "refs.validate", "REF_DIVERGED: projection differs from authoritative manifest", nil, "ref", ref)
		}
	}
	for ref := range refs {
		if !ownedNamespace(ref) || ref == stateRef || excludedOperationalRef(ref) || (allowJournal && ref == journalRef) {
			continue
		}
		if expected[ref] == "" {
			return diag.New("conflict", "refs.validate", "REF_DIVERGED: unexpected exporter-owned ref", nil, "ref", ref)
		}
	}
	return nil
}

// These explicitly reserved namespaces never participate in canonical equality.
// The active publication journal remains subject to the stricter checks above.
func excludedOperationalRef(ref string) bool {
	return strings.HasPrefix(ref, "refs/cvsgitrelay/backup/") || (strings.HasPrefix(ref, "refs/cvsgitrelay/ops/") && ref != journalRef)
}

func (r *Repository) loadCurrent(ctx context.Context) error {
	refs, err := r.allRefs(ctx)
	if err != nil {
		return err
	}
	marked, err := r.hasPublicationMarker()
	if err != nil {
		return err
	}
	if refs[stateRef] == "" && marked {
		return diag.New("integrity", "state.read", "missing authoritative state ref after a publication attempt; recovery evidence is required", nil)
	}
	m := r.Manifest
	// Open validated this generation under the same writer lock moments ago.
	// Skip only that one repeat; later loads always validate in full.
	if !r.openedState || refs[stateRef] != r.StateOID {
		if m, err = r.readState(ctx, refs[stateRef]); err != nil {
			return err
		}
	}
	r.openedState = false
	if err := validateProjections(refs, refs[stateRef], m.Refs, false); err != nil {
		return err
	}
	r.StateOID = refs[stateRef]
	r.Manifest = m
	return nil
}

type publicationIntent struct {
	Version           int    `json:"version"`
	Phase             string `json:"phase"`
	Old, New          string
	Expected, Desired map[string]string
}
type publicationJournal struct {
	Intent   publicationIntent
	Checksum string
}

func journalObjects(intent publicationIntent) (canonical.Objects, string, error) {
	data, err := json.Marshal(intent)
	if err != nil {
		return nil, "", err
	}
	journal := publicationJournal{intent, canonical.Hash("cvsgitrelay-publication-journal-v2", data)}
	data, err = json.Marshal(journal)
	if err != nil {
		return nil, "", err
	}
	o := canonical.Objects{}
	blob := o.Put("blob", data)
	tree, err := o.Tree(map[string]string{"intent.json": blob})
	if err != nil {
		return nil, "", err
	}
	parents := []string{intent.New}
	if intent.Old != "" && intent.Old != intent.New {
		parents = append(parents, intent.Old)
	}
	return o, o.Synthetic(tree, parents, "cvsgitrelay-publication-intent-v2\n"), nil
}

func (r *Repository) validateIntent(ctx context.Context, journal string) (publicationIntent, error) {
	var j publicationJournal
	data, err := r.git(ctx, nil, nil, "show", journal+":intent.json")
	if err != nil {
		return j.Intent, err
	}
	if err := decodeJSON(data, &j); err != nil {
		return j.Intent, err
	}
	i := j.Intent
	payload, err := json.Marshal(i)
	if err != nil {
		return i, err
	}
	if i.Version != 2 || i.Phase != "prepared" || i.New == i.Old || !validOID(i.New) || (i.Old != "" && !validOID(i.Old)) || j.Checksum != canonical.Hash("cvsgitrelay-publication-journal-v2", payload) {
		return i, fmt.Errorf("invalid journal checksum/schema")
	}
	old, err := r.readState(ctx, i.Old)
	if err != nil {
		return i, err
	}
	next, err := r.readState(ctx, i.New)
	if err != nil {
		return i, err
	}
	if !maps.Equal(old.Refs, i.Expected) || !maps.Equal(next.Refs, i.Desired) {
		return i, fmt.Errorf("journal differs from state manifests")
	}
	o, expected, err := journalObjects(i)
	if err != nil || expected != journal {
		return i, fmt.Errorf("invalid journal envelope")
	}
	if err := r.compareObjects(ctx, o); err != nil {
		return i, fmt.Errorf("journal object differs: %w", err)
	}
	return i, nil
}

// Every recovery/projector step rechecks the authoritative generation and all
// owned refs while holding the same writer lock. Unknown values are never forced.
func (r *Repository) reconcile(ctx context.Context, i publicationIntent, journal string, opts *Options) error {
	union := maps.Clone(i.Expected)
	for ref, oid := range i.Desired {
		union[ref] = oid
	}
	for _, ref := range canonical.SortedKeys(union) {
		refs, err := r.allRefs(ctx)
		if err != nil {
			return err
		}
		if refs[stateRef] != i.New || refs[journalRef] != journal {
			return diag.New("conflict", "recovery.state", "state or recovery evidence changed", nil)
		}
		for name, value := range refs {
			if !ownedNamespace(name) || name == stateRef || name == journalRef || excludedOperationalRef(name) {
				continue
			}
			if _, ok := union[name]; !ok {
				return diag.New("conflict", "recovery.refs", "REF_DIVERGED: unexpected owned ref", nil, "ref", name)
			}
			if value != i.Expected[name] && value != i.Desired[name] {
				return diag.New("conflict", "recovery.refs", "REF_DIVERGED: projection equals neither recorded old nor new value", nil, "ref", name)
			}
		}
		for name := range union {
			if refs[name] != i.Expected[name] && refs[name] != i.Desired[name] {
				return diag.New("conflict", "recovery.refs", "REF_DIVERGED: missing or changed projection", nil, "ref", name)
			}
		}
		if refs[ref] != i.Desired[ref] {
			if err := r.evidenceCAS(ctx, ref, refs[ref], i.Desired[ref], i.New, journal, nil); err != nil {
				return err
			}
			if opts != nil {
				if err := opts.hit("projection-updated"); err != nil {
					return err
				}
			}
		}
	}
	refs, err := r.allRefs(ctx)
	if err != nil {
		return err
	}
	if err := validateProjections(refs, i.New, i.Desired, true); err != nil {
		return err
	}
	if opts != nil {
		if err := opts.hit("before-journal-cleanup"); err != nil {
			return err
		}
	}
	return r.evidenceCAS(ctx, journalRef, journal, "", i.New, journal, projectionChecks(i, true))
}

func (r *Repository) recoverPublication(ctx context.Context) error {
	refs, err := r.allRefs(ctx)
	if err != nil {
		return err
	}
	journal := refs[journalRef]
	if journal == "" {
		return nil
	}
	i, err := r.validateIntent(ctx, journal)
	if err != nil {
		return diag.New("integrity", "recovery.journal", "invalid publication evidence; recovery stopped", err)
	}
	refs, err = r.allRefs(ctx)
	if err != nil {
		return err
	}
	if refs[journalRef] != journal {
		return diag.New("conflict", "recovery.journal", "publication evidence changed during validation", nil)
	}
	switch refs[stateRef] {
	case i.Old:
		if err := validateProjections(refs, i.Old, i.Expected, true); err != nil {
			return err
		}
		if i.Old == "" {
			if _, err := r.hasPublicationMarker(); err != nil {
				return err
			}
		}
		if err := r.evidenceCAS(ctx, journalRef, journal, "", i.Old, journal, projectionChecks(i, false)); err != nil {
			return err
		}
		// A validated old=nil intent proves the first state never committed.
		// Without that journal, a witness plus a missing state fails closed.
		if i.Old == "" {
			if err := os.Remove(r.publicationMarker()); err != nil && !os.IsNotExist(err) {
				return err
			}
			dir, err := os.Open(r.Path)
			if err != nil {
				return err
			}
			defer dir.Close()
			return dir.Sync()
		}
		return nil
	case i.New:
		return r.reconcile(ctx, i, journal, nil)
	default:
		return diag.New("integrity", "recovery.state", "state matches neither journal generation; recovery stopped", nil)
	}
}

func (r *Repository) publishGeneration(ctx context.Context, g canonical.Generation, opts *Options) error {
	if err := r.recoverPublication(ctx); err != nil {
		return err
	}
	refs, err := r.allRefs(ctx)
	if err != nil {
		return err
	}
	if err := validateProjections(refs, r.StateOID, r.Manifest.Refs, false); err != nil {
		return err
	}
	if g.State == r.StateOID {
		return nil
	}
	if _, err := r.storeObjects(ctx, g.Objects, opts); err != nil {
		return err
	}
	if _, err := r.readState(ctx, g.State); err != nil {
		return err
	}
	if err := opts.hit("objects-written"); err != nil {
		return err
	}
	i := publicationIntent{Version: 2, Phase: "prepared", Old: r.StateOID, New: g.State, Expected: maps.Clone(r.Manifest.Refs), Desired: maps.Clone(g.Manifest.Refs)}
	o, journal, err := journalObjects(i)
	if err != nil {
		return err
	}
	if _, err := r.storeObjects(ctx, o, nil); err != nil {
		return err
	}
	if err := opts.hit("before-journal"); err != nil {
		return err
	}
	if err := r.cas(ctx, journalRef, "", journal); err != nil {
		return err
	}
	if err := opts.hit("journal-written"); err != nil {
		return err
	}
	if err := r.ensurePublicationMarker(); err != nil {
		return err
	}
	if err := opts.hit("before-publish"); err != nil {
		return err
	}
	refs, err = r.allRefs(ctx)
	if err != nil {
		return err
	}
	if err := validateProjections(refs, r.StateOID, r.Manifest.Refs, true); err != nil {
		return err
	}
	if err := r.evidenceCAS(ctx, stateRef, r.StateOID, g.State, r.StateOID, journal, projectionChecks(i, false)); err != nil {
		return err
	}
	if err := opts.hit("after-publish"); err != nil {
		return err
	}
	if err := r.reconcile(ctx, i, journal, opts); err != nil {
		return err
	}
	r.StateOID = g.State
	r.Manifest = g.Manifest
	return nil
}
