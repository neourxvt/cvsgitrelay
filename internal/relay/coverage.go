package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
)

type CoverageSummary struct {
	PreviousRevisions int      `json:"previous_revisions"`
	CurrentRevisions  int      `json:"current_revisions"`
	Missing           []string `json:"missing_path_revision_hex"`
	ContentChanged    []string `json:"content_changed_path_revision_hex"`
}

// CompareCoverage supports the preferred upgrade: build a new v2 destination,
// then compare revision coverage and live bytes without altering either mirror.
// This small v1 provenance reader is not an import or migration execution path.
func CompareCoverage(ctx context.Context, runner *process.Runner, previous, current string) (CoverageSummary, error) {
	var summary CoverageSummary
	next, err := openRepository(ctx, runner, current, false)
	if err != nil {
		return summary, err
	}
	defer next.Close()
	previous, err = filepath.Abs(previous)
	if err != nil {
		return summary, err
	}
	if previous == next.Path {
		return summary, diag.New("config", "coverage.open", "previous and new destinations must differ", nil)
	}
	old := &Repository{Path: previous, Runner: runner}
	if err := old.acquire(); err != nil {
		return summary, err
	}
	defer old.Close()
	if err := old.validateBare(ctx); err != nil {
		return summary, err
	}
	config, err := os.ReadFile(filepath.Join(previous, "cvsgitrelay.json"))
	if err != nil {
		return summary, err
	}
	var identity struct {
		Version int    `json:"format_version"`
		Source  string `json:"source_id"`
	}
	if err := json.Unmarshal(config, &identity); err != nil {
		return summary, err
	}
	if identity.Version != 1 || identity.Source == "" {
		return summary, diag.New("unsupported", "coverage.open", "previous destination must be a v1 mirror", nil)
	}
	refs, err := old.allRefs(ctx)
	if err != nil {
		return summary, err
	}
	if refs[stateRef] == "" {
		return summary, diag.New("integrity", "coverage.state", "previous state missing", nil)
	}
	data, err := old.git(ctx, nil, nil, "show", refs[stateRef]+":manifest.json")
	if err != nil {
		return summary, err
	}
	var manifest struct {
		Version int               `json:"version"`
		Refs    map[string]string `json:"refs"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return summary, err
	}
	if manifest.Version != 1 || manifest.Refs == nil {
		return summary, diag.New("integrity", "coverage.state", "invalid v1 manifest", nil)
	}
	for ref, oid := range manifest.Refs {
		if !validOID(oid) || (ref != trunkRef && !strings.HasPrefix(ref, "refs/heads/cvs/branches/") && !strings.HasPrefix(ref, "refs/cvsgitrelay/unmapped/")) {
			return summary, diag.New("integrity", "coverage.state", "invalid v1 data ref", nil)
		}
	}
	if err := validateProjections(refs, refs[stateRef], manifest.Refs, false); err != nil {
		return summary, err
	}
	// The v2 manifest uses physical archive identities; v1 used checkout paths.
	currentEvents := map[string]canonical.RevisionEntry{}
	for encoded, entry := range next.Manifest.Revisions {
		k, err := canonical.DecodeKey([]byte(encoded))
		if err != nil {
			return summary, err
		}
		for _, mapping := range next.Config.Policy.Mappings {
			if strings.HasPrefix(k.Archive, mapping.Archive+"/") {
				path := strings.TrimPrefix(k.Archive, mapping.Archive+"/")
				if mapping.Checkout != "" {
					path = mapping.Checkout + "/" + path
				}
				currentEvents[key(path, k.Number)] = entry
			}
		}
	}
	summary.CurrentRevisions = len(currentEvents)
	args := []string{"rev-list"}
	for _, ref := range canonical.SortedKeys(manifest.Refs) {
		args = append(args, manifest.Refs[ref])
	}
	if len(args) == 1 {
		return summary, nil
	}
	out, err := old.git(ctx, nil, nil, args...)
	if err != nil {
		return summary, err
	}
	seen := map[string]bool{}
	for _, oid := range strings.Fields(string(out)) {
		data, err := old.git(ctx, nil, nil, "cat-file", "commit", oid)
		if err != nil {
			return summary, err
		}
		_, body, ok := bytes.Cut(data, []byte("\n\n"))
		if !ok {
			return summary, diag.New("integrity", "coverage.provenance", "invalid v1 commit", nil)
		}
		header, _, ok := bytes.Cut(body, []byte("\n\n"))
		fields := strings.Split(string(header), "\n")
		if !ok || len(fields) != 4 || fields[0] != "cvsgitrelay-Version: 1" || fields[1] != "cvsgitrelay-Source-ID: "+identity.Source {
			return summary, diag.New("integrity", "coverage.provenance", "mismatched v1 source/provenance", nil)
		}
		if fields[2] == "cvsgitrelay-Kind: seed" {
			continue
		}
		if fields[2] != "cvsgitrelay-Kind: revision" || !strings.HasPrefix(fields[3], "cvsgitrelay-Data: ") {
			return summary, diag.New("integrity", "coverage.provenance", "unsupported v1 event", nil)
		}
		encoded := strings.TrimPrefix(fields[3], "cvsgitrelay-Data: ")
		meta, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return summary, err
		}
		var revision struct {
			Path   string `json:"path_hex"`
			Number string `json:"revision"`
			State  string `json:"state"`
			Blob   string `json:"blob"`
		}
		if err := json.Unmarshal(meta, &revision); err != nil {
			return summary, err
		}
		path, err := hex.DecodeString(revision.Path)
		if err != nil {
			return summary, err
		}
		if err := canonical.Path(string(path)); err != nil {
			return summary, err
		}
		if _, err := canonical.Number(revision.Number); err != nil {
			return summary, err
		}
		id := key(string(path), revision.Number)
		if seen[id] {
			return summary, diag.New("integrity", "coverage.provenance", "duplicate v1 revision identity", nil)
		}
		seen[id] = true
		summary.PreviousRevisions++
		entry, ok := currentEvents[id]
		if !ok {
			summary.Missing = append(summary.Missing, hexBytes(id))
			continue
		}
		message, err := next.git(ctx, nil, nil, "cat-file", "commit", entry.Event)
		if err != nil {
			return summary, err
		}
		_, message, ok = bytes.Cut(message, []byte("\n\n"))
		if !ok {
			return summary, diag.New("integrity", "coverage.provenance", "invalid v2 event", nil)
		}
		record, err := canonical.ParseRevisionMessage(message)
		if err != nil {
			return summary, err
		}
		if revision.State == "dead" {
			entries, err := old.git(ctx, nil, nil, "--literal-pathspecs", "ls-tree", "-z", oid, "--", string(path))
			if err != nil {
				return summary, err
			}
			if len(entries) != 0 {
				return summary, diag.New("integrity", "coverage.provenance", "v1 deletion event still contains its file", nil)
			}
			if record.State != "dead" {
				summary.ContentChanged = append(summary.ContentChanged, hexBytes(id))
			}
			continue
		}
		if revision.State != "Exp" || !validOID(revision.Blob) {
			return summary, diag.New("integrity", "coverage.provenance", "unsupported v1 state/blob", nil)
		}
		if record.State != "live" {
			summary.ContentChanged = append(summary.ContentChanged, hexBytes(id))
			continue
		}
		previousContent, err := old.git(ctx, nil, nil, "show", oid+":"+string(path))
		if err != nil {
			return summary, err
		}
		if canonical.OID("blob", previousContent) != revision.Blob {
			return summary, diag.New("integrity", "coverage.provenance", "v1 tree bytes differ from recorded blob", nil)
		}
		content, err := next.git(ctx, nil, nil, "show", entry.Event+":"+string(path))
		if err != nil {
			return summary, err
		}
		if !bytes.Equal(content, previousContent) {
			summary.ContentChanged = append(summary.ContentChanged, hexBytes(id))
		}
	}
	slices.Sort(summary.Missing)
	slices.Sort(summary.ContentChanged)
	if len(summary.Missing) > 0 || len(summary.ContentChanged) > 0 {
		return summary, diag.New("integrity", "coverage.compare", "new destination is missing previous revisions or has different live bytes/deletion states", nil)
	}
	return summary, nil
}
