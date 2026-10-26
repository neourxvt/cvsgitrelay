package relay

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"

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
	var in canonical.Inventory
	var stats sourceStats
	listed, err := r.scan(ctx)
	if err != nil {
		return in, stats, err
	}
	if err := validateReportedAncestry(listed); err != nil {
		return in, stats, err
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

	// Dead checkout output cannot distinguish an inaccessible body from an
	// actual empty file. The profile always encodes dead content as absent.
	// Live contents come from exact numeric -kb checkouts, without filters.
	var fetches []*Revision
	var positions []int
	for i := range in.Records {
		record := &in.Records[i]
		if !record.Present || r.reuseTrustedContent(record) {
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
	}
	in, err = in.Normalize(r.Config.Policy)
	if err != nil {
		return in, stats, diag.New("source", "source.normalize", "invalid normalized source inventory", err)
	}
	return in, stats, nil
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
	current := map[string]canonical.Record{}
	for _, record := range in.Records {
		current[record.Key.ID()] = record
	}
	for key, old := range r.Manifest.Revisions {
		record, ok := current[key]
		if !ok {
			return diag.New("source", "source.verify", "SOURCE_MISSING: previously accepted revision disappeared", nil, "key_hex", hexBytes(key))
		}
		if record.Fingerprint() != old.Fingerprint {
			return diag.New("source", "source.verify", "SOURCE_MUTATED: immutable revision record changed", nil, "key_hex", hexBytes(key))
		}
	}
	return nil
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

// Inventory acceptance compares two complete scans, including normalized bytes.
// This detects observed instability, without claiming an atomic CVS snapshot.
func (r *Repository) acceptSource(ctx context.Context, opts *Options) (canonical.Inventory, sourceStats, error) {
	var stats sourceStats
	if err := opts.hit("scan-start"); err != nil {
		return canonical.Inventory{}, stats, err
	}
	// Keep slow remote enumeration and content reads visible independently of
	// subprocess completion. Progress never enters canonical source records.
	if r.Runner.Log != nil {
		progress := newMirrorProgress(ctx, r.Runner.Log, opts.ProgressInterval)
		progress.stage("Verifying complete CVS source", "verification_policy", r.Config.Policy.Verify)
		defer progress.close()
	}
	reader := func(ctx context.Context) (canonical.Inventory, sourceStats, error) {
		return r.readSourceScope(ctx, opts.Connections)
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
	r.reuseTrusted(&first)
	if err := r.verifyAccepted(first); err != nil {
		return first, stats, err
	}
	if err := opts.hit("fetch-complete"); err != nil {
		return first, stats, err
	}
	second, b, err := reader(ctx)
	stats.Bytes += b.Bytes
	stats.Requests += b.Requests
	if err != nil {
		return first, stats, err
	}
	r.reuseTrusted(&second)
	if err := r.verifyAccepted(second); err != nil {
		return first, stats, err
	}
	if first.Digest(r.Config.SourceID) != second.Digest(r.Config.SourceID) {
		return first, stats, diag.New("source", "source.accept", "source changed between complete scans; retry when stable", nil)
	}
	stats.Requests += r.metadataRequests - metadataRequests
	stats.Bytes += r.metadataBytes - metadataBytes
	if err := opts.hit("scan-complete"); err != nil {
		return first, stats, err
	}
	if r.Runner.Log != nil {
		r.Runner.Log.Info("Source verified", "verification_policy", r.Config.Policy.Verify, "revisions", len(first.Records), "source_requests", stats.Requests, "source_bytes", stats.Bytes)
	}
	return first, stats, nil
}

// ValidateRequestedPolicy prevents a command-line policy change from silently
// reinterpreting already accepted source records or the verification guarantee.
func (r *Repository) ValidateRequestedPolicy(source, verify string) error {
	if source != r.Config.SourceID || verify != r.Config.Policy.Verify {
		return diag.New("config", "source.policy", "source identity or verification policy differs from format v2 destination; use a new destination", nil)
	}
	return nil
}
