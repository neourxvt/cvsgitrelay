package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

type sourceStats struct {
	Bytes    int64
	Requests int
}

// readSourceScope uses only ordinary CVS reads, even for a local CVSROOT.
// One normalization profile therefore applies across transport aliases; no
// filesystem access to the server's RCS archives is required or attempted.
func (r *Repository) readSourceScope(ctx context.Context, connections int) (canonical.Inventory, sourceStats, error) {
	return r.readSharedSourceScope(ctx, connections, nil)
}

// previous maps record IDs to the contents of an earlier complete scan. Equal
// bytes reuse that slice as soon as they are read, so a confirming scan does
// not keep a second copy of the whole revision history alive.
func (r *Repository) readSharedSourceScope(ctx context.Context, connections int, previous map[string][]byte) (canonical.Inventory, sourceStats, error) {
	var stats sourceStats
	in, listed, err := r.readSourceMetadata(ctx)
	if err != nil {
		return in, stats, err
	}

	// Dead checkout output cannot distinguish an inaccessible body from an
	// actual empty file. The profile always encodes dead content as absent.
	// Live contents come from exact numeric -kb checkouts, without filters.
	share := func(record *canonical.Record) {
		if old, ok := previous[record.Key.ID()]; ok && bytes.Equal(old, record.Content) {
			record.Content = old
		}
	}
	var fetches []*Revision
	var positions []int
	for i := range in.Records {
		record := &in.Records[i]
		if !record.Present {
			continue
		}
		if r.reuseTrustedContent(record) {
			share(record)
			continue
		}
		positions = append(positions, i)
		fetches = append(fetches, listed.Revisions[key(record.Path, record.Key.Number)])
	}
	pending := newAllRevisionFetches(ctx, fetches, max(1, connections), r.fetch)
	defer pending.close()
	for _, i := range positions {
		data, err := pending.next()
		stats.Requests++
		stats.Bytes += int64(len(data))
		if err != nil {
			return in, stats, err
		}
		in.Records[i].Content = data
		share(&in.Records[i])
	}
	in, err = in.Normalize(r.Config.Policy)
	if err != nil {
		return in, stats, diag.New("source", "source.normalize", "invalid normalized source inventory", err)
	}
	return in, stats, nil
}

// readSourceMetadata enumerates every revision record and symbol without any
// contents. Live records are marked present; their bodies are not read here.
func (r *Repository) readSourceMetadata(ctx context.Context) (canonical.Inventory, *Inventory, error) {
	var in canonical.Inventory
	listed, err := r.scan(ctx)
	if err != nil {
		return in, nil, err
	}
	if err := validateReportedAncestry(listed); err != nil {
		return in, nil, err
	}
	for _, revision := range listed.Ordered {
		state := "live"
		if revision.State == "dead" {
			state = "dead"
		}
		record := canonical.Record{
			Key:  canonical.Key{Source: r.Config.SourceID, Archive: revision.SourcePath, Number: revision.Number},
			Path: revision.Path, Time: revision.Time, Author: revision.Author,
			Log: revision.Log, State: state, Present: state == "live",
		}
		if revision.Predecessor != "" {
			parent := canonical.Key{Source: r.Config.SourceID, Archive: revision.SourcePath, Number: revision.Predecessor}
			record.Parent = &parent
		}
		in.Records = append(in.Records, record)
	}
	for _, file := range listed.Files {
		for name, target := range file.Symbols {
			symbol := canonical.Symbol{Archive: file.SourcePath, Name: name, Target: target}
			if branch, ok := canonical.Branch(target); ok {
				symbol.Branch, symbol.Target = true, branch
			}
			in.Symbols = append(in.Symbols, symbol)
		}
	}
	return in, listed, nil
}

// GNU CVS reports complete numeric lines and the branch numbers originating
// at each forkpoint. Their combination establishes the supported logical RCS
// ancestry, including gaps, without inventing an inaccessible numeric parent.
// Raw storage delta links are not exposed by CVS and are not claimed here.
func validateReportedAncestry(in *Inventory) error {
	for _, file := range in.Files {
		declared := map[string]bool{}
		actual := map[string]bool{}
		for _, revision := range file.Revisions {
			if revision.Line != "trunk" {
				actual[revision.Line] = true
			}
			for _, branch := range revision.Branches {
				line, ok := canonical.Branch(branch)
				if !ok || line != branch || canonical.Fork(line) != revision.Number || declared[line] {
					return diag.New("source", "source.ancestry", "ambiguous branch declaration", nil, "archive", file.SourcePath, "branch", branch)
				}
				declared[line] = true
			}
		}
		if !maps.Equal(actual, declared) {
			return diag.New("source", "source.ancestry", "branch declarations disagree with the complete revision listing", nil, "archive", file.SourcePath)
		}
	}
	return nil
}

// Accepted fingerprints are sufficient to compare every immutable field after
// restart; operational cache/journal contents have no authority over acceptance.
func (r *Repository) verifyAccepted(in canonical.Inventory) error {
	current, err := r.verifyListed(in)
	if err != nil {
		return err
	}
	for key, old := range r.Manifest.Revisions {
		if current[key].Fingerprint() != old.Fingerprint {
			return diag.New("source", "source.verify", "SOURCE_MUTATED: immutable revision record changed", nil, "key_hex", hexBytes(key))
		}
	}
	return nil
}

// verifyListed requires every accepted revision to still be listed. It needs
// metadata only and returns the current records by ID.
func (r *Repository) verifyListed(in canonical.Inventory) (map[string]canonical.Record, error) {
	current := map[string]canonical.Record{}
	for _, record := range in.Records {
		current[record.Key.ID()] = record
	}
	for _, key := range canonical.SortedKeys(r.Manifest.Revisions) {
		if _, ok := current[key]; !ok {
			return nil, diag.New("source", "source.verify", "SOURCE_MISSING: previously accepted revision disappeared", nil, "key_hex", hexBytes(key))
		}
	}
	return current, nil
}

// Trust mode may reuse only a complete record matching the authoritative
// fingerprint. Cache loss or damage falls back to freshly read source bytes.
// Metadata is always re-enumerated and compared before content is substituted.
func (r *Repository) reuseTrusted(in *canonical.Inventory) {
	for i := range in.Records {
		r.reuseTrustedContent(&in.Records[i])
	}
}

// A cached record must authenticate against canonical state before its content
// can replace a remote read. Freshly scanned immutable metadata is retained.
func (r *Repository) reuseTrustedContent(current *canonical.Record) bool {
	if r.Config.Policy.Verify != "trust-append-only" {
		return false
	}
	old, ok := r.Manifest.Revisions[current.Key.ID()]
	if !ok {
		return false
	}
	path := filepath.Join(r.Path, "cvsgitrelay-cache", digest(current.Key.Bytes())+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cached canonical.Record
	if decodeJSON(data, &cached) != nil || cached.Key != current.Key || cached.Present != current.Present || cached.Fingerprint() != old.Fingerprint {
		return false
	}
	current.Content = cached.Content
	return true
}

func (r *Repository) cacheAccepted(in canonical.Inventory) error {
	if r.Config.Policy.Verify != "trust-append-only" {
		return nil
	}
	dir := filepath.Join(r.Path, "cvsgitrelay-cache")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, record := range in.Records {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, digest(record.Key.Bytes())+".json")
		tmp, err := os.CreateTemp(dir, ".record-")
		if err != nil {
			return err
		}
		name := tmp.Name()
		_, err = tmp.Write(data)
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(name, path)
		}
		if err != nil {
			os.Remove(name)
			return err
		}
	}
	return nil
}

// sourceConnections returns the number of concurrent CVS reads. The configured
// limit protects a remote server; a local CVSROOT has no connections to limit,
// and every read is an independent local cvs process, so all cores are used.
func sourceConnections(root string, configured int) int {
	if localCVSRoot(root) {
		return runtime.NumCPU()
	}
	return configured
}

// stableMirrorSource takes a shared lock on the writer lock of a local
// cvsgitrelay mirror. mirror holds that lock exclusively for its whole run and
// is the only writer of a mirror, so while the shared lock is held the archives
// cannot change. ok is false for every other source, including mirrors without
// a readable lock file; those keep the confirming second scan.
func stableMirrorSource(root string) (release func(), ok bool, err error) {
	if !localCVSRoot(root) {
		return nil, false, nil
	}
	dir, err := sourceDirectory(root)
	if err != nil {
		return nil, false, err
	}
	lock, err := os.Open(filepath.Join(dir, mirrorStateDir, "lock"))
	if err != nil {
		return nil, false, nil
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, diag.New("locked", "source.mirror", "the local CVS mirror is being updated; retry after mirror finishes", err)
		}
		return nil, false, nil
	}
	return func() { lock.Close() }, true, nil
}

// Inventory acceptance compares two complete scans, including normalized bytes.
// This detects observed instability, without claiming an atomic CVS snapshot.
// A locked local cvsgitrelay mirror cannot change during the read, so one
// complete scan is accepted without the confirming second one.
func (r *Repository) acceptSource(ctx context.Context, opts *Options) (canonical.Inventory, sourceStats, error) {
	var stats sourceStats
	if err := opts.hit("scan-start"); err != nil {
		return canonical.Inventory{}, stats, err
	}
	release, stable, err := stableMirrorSource(r.Config.CVSRoot)
	if err != nil {
		return canonical.Inventory{}, stats, err
	}
	if stable {
		defer release()
	}
	scans := 2
	if stable {
		scans = 1
	}
	// Keep slow remote enumeration and content reads visible independently of
	// subprocess completion. Progress never enters canonical source records.
	connections := sourceConnections(r.Config.CVSRoot, opts.Connections)
	r.phases.begin("read_source", "Verifying complete CVS source", "verification_policy", r.Config.Policy.Verify, "connections", connections, "scans", scans)
	var previous map[string][]byte
	reader := func(ctx context.Context) (canonical.Inventory, sourceStats, error) {
		return r.readSharedSourceScope(ctx, connections, previous)
	}
	metadataRequests, metadataBytes := r.metadataRequests, r.metadataBytes
	if r.sourceReader != nil {
		reader = r.sourceReader
	}
	first, a, err := reader(ctx)
	stats.Bytes += a.Bytes
	stats.Requests += a.Requests
	if err != nil {
		return first, stats, err
	}
	// readSharedSourceScope already substitutes trusted cached content while
	// reading; only injected readers need the separate pass.
	if r.sourceReader != nil {
		r.reuseTrusted(&first)
	}
	if err := r.verifyAccepted(first); err != nil {
		return first, stats, err
	}
	if err := opts.hit("fetch-complete"); err != nil {
		return first, stats, err
	}
	if !stable {
		previous = map[string][]byte{}
		for _, record := range first.Records {
			if record.Present {
				previous[record.Key.ID()] = record.Content
			}
		}
		second, b, err := reader(ctx)
		stats.Bytes += b.Bytes
		stats.Requests += b.Requests
		if err != nil {
			return first, stats, err
		}
		previous = nil
		if r.sourceReader != nil {
			r.reuseTrusted(&second)
		}
		if err := r.verifyAccepted(second); err != nil {
			return first, stats, err
		}
		if first.Digest(r.Config.SourceID) != second.Digest(r.Config.SourceID) {
			return first, stats, diag.New("source", "source.accept", "source changed between complete scans; retry when stable", nil)
		}
	}
	stats.Requests += r.metadataRequests - metadataRequests
	stats.Bytes += r.metadataBytes - metadataBytes
	if err := opts.hit("scan-complete"); err != nil {
		return first, stats, err
	}
	if r.Runner.Log != nil {
		r.Runner.Log.Info("Source verified", "verification_policy", r.Config.Policy.Verify, "scans", scans, "revisions", len(first.Records), "source_requests", stats.Requests, "source_bytes", stats.Bytes)
	}
	return first, stats, nil
}

// ValidateRequestedPolicy prevents a command-line policy change from silently
// reinterpreting already accepted source records or the verification guarantee.
// The author map is fixed at init because it is part of every event commit,
// and the branch map because it names every published branch ref.
func (r *Repository) ValidateRequestedPolicy(source, verify string, authors, branches map[string]string) error {
	if source != r.Config.SourceID || verify != r.Config.Policy.Verify {
		return diag.New("config", "source.policy", "source identity or verification policy differs from the destination; use a new destination", nil)
	}
	if !maps.Equal(authors, r.Config.Policy.Authors) {
		if r.Config.Version == 2 {
			return diag.New("config", "source.policy", "author maps require a format 3 destination; use a new destination", nil)
		}
		return diag.New("config", "source.policy", "author map differs from the destination; use a new destination", nil)
	}
	if !maps.Equal(branches, r.Config.Policy.Branches) && (len(branches) > 0 || len(r.Config.Policy.Branches) > 0) {
		return diag.New("config", "source.policy", "branch map differs from the destination; use a new destination", nil)
	}
	return nil
}
