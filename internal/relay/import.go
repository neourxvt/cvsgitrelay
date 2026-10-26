package relay

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cvsgitrelay/internal/diag"
)

type Options struct {
	Scope            string
	Mode             string
	Failpoint        string
	ProgressInterval time.Duration
	faultCounts      map[string]int
}

type Summary struct {
	Operation        string   `json:"operation"`
	Scope            string   `json:"scope"`
	Files            int      `json:"files"`
	Discovered       int      `json:"discovered"`
	Imported         int      `json:"imported"`
	Pending          int      `json:"pending"`
	Trunk            int      `json:"trunk"`
	Nontrunk         int      `json:"nontrunk"`
	Vendor           int      `json:"vendor"`
	Dead             int      `json:"dead"`
	Published        int      `json:"published"`
	Unimported       int      `json:"unimported"`
	Refs             int      `json:"refs"`
	NamedBranches    int      `json:"named_branches"`
	ArchivalRefs     int      `json:"archival_refs"`
	ScopeLimitations []string `json:"scope_limitations,omitempty"`
}

func (r *Repository) inspect(ctx context.Context) (*Inventory, map[string]*Imported, map[string][]*Imported, error) {
	imports, chains, err := r.reconstruct(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	in, err := r.scan(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := r.validatePublished(ctx, in, imports, chains); err != nil {
		return nil, nil, nil, err
	}
	return in, imports, chains, nil
}

func (r *Repository) validatePublished(ctx context.Context, in *Inventory, imports map[string]*Imported, chains map[string][]*Imported) error {
	for k, c := range imports {
		rev := in.Revisions[k]
		if rev == nil {
			return diag.New("integrity", "source.compare", "published revision disappeared upstream; history removal or incomplete scan", nil, "path_hex", c.Meta.PathHex, "revision", c.Meta.Revision, "commit", c.OID)
		}
		expected := metadata(rev, in.Files[rev.Path], c.Meta.Blob)
		// CVS clears a file's default vendor branch during an ordinary trunk
		// check-in. This field records what was observed at import time; it is
		// not immutable per-revision metadata or the revision's destination.
		expected.DefaultBranch = c.Meta.DefaultBranch
		a, _ := json.Marshal(expected)
		b, _ := json.Marshal(c.Meta)
		if string(a) != string(b) {
			return diag.New("integrity", "source.compare", "published revision metadata, ancestry or branch mapping changed", nil, "path_hex", c.Meta.PathHex, "revision", c.Meta.Revision, "commit", c.OID, "expected_metadata", expected, "published_metadata", c.Meta)
		}
		if rev.Predecessor != "" && imports[key(rev.Path, rev.Predecessor)] == nil {
			return diag.New("integrity", "source.compare", "previously missed predecessor now appears before a published revision", nil, "path_hex", c.Meta.PathHex, "revision", c.Meta.Revision, "predecessor", rev.Predecessor)
		}
		if err := r.checkTransition(ctx, c); err != nil {
			return err
		}
	}
	for ref, chain := range chains {
		last := map[string]string{}
		ordered := slices.Clone(chain)
		slices.Reverse(ordered)
		for _, c := range ordered {
			if c.Kind == "seed" {
				continue
			}
			rev := in.Revisions[key(mustPath(c.Meta.PathHex), c.Meta.Revision)]
			previous := last[rev.Path]
			if previous != "" && previous != rev.Predecessor {
				return diag.New("integrity", "history.order", "same-file ancestry is out of order", nil, "ref", ref, "commit", c.OID, "previous_revision", previous, "predecessor", rev.Predecessor)
			}
			last[rev.Path] = rev.Number
		}
	}
	return r.validateBranches(ctx, in, imports, chains)
}

func mustPath(s string) string { p, _ := decodePath(s); return p }

func summarize(op, scope string, in *Inventory, imports map[string]*Imported, refs map[string]string) Summary {
	s := Summary{Operation: op, Scope: scope, Files: len(in.Files), Discovered: len(in.Revisions), Imported: len(imports), Trunk: in.Trunk, Nontrunk: len(in.Revisions) - in.Trunk, Vendor: in.Vendor, Dead: in.Dead, Refs: len(refs)}
	for ref := range refs {
		if ref == trunkRef {
			continue
		}
		if branchKind(ref) == "archive" {
			s.ArchivalRefs++
		} else {
			s.NamedBranches++
		}
	}
	if s.ArchivalRefs > 0 {
		s.ScopeLimitations = append(s.ScopeLimitations, "unnamed refs archive individual file revision lines; their trees are not project checkouts")
	}
	if scope == "trunk" {
		s.ScopeLimitations = append(s.ScopeLimitations, "trunk scope excludes nontrunk revisions from the requested import and verification")
	}
	for _, file := range in.Files {
		if file.DefaultBranch != "" {
			s.ScopeLimitations = append(s.ScopeLimitations, "trunk means numeric 1.N revisions; CVS default vendor checkout behavior is separate")
			break
		}
	}
	for k, rev := range in.Revisions {
		if imports[k] == nil {
			s.Unimported++
			if scope == "all" || rev.Line == "trunk" {
				s.Pending++
			}
		}
	}
	return s
}

func (r *Repository) Status(ctx context.Context, scope string) (Summary, error) {
	in, imports, _, err := r.inspect(ctx)
	if err != nil {
		return Summary{}, err
	}
	return summarize("status", scope, in, imports, r.Manifest.Refs), nil
}

func (r *Repository) Sync(ctx context.Context, opts Options) (Summary, error) {
	if err := opts.hit("scan-start"); err != nil {
		return Summary{}, err
	}
	in, imports, _, err := r.inspect(ctx)
	if err != nil {
		return Summary{}, err
	}
	if err := opts.hit("scan-complete"); err != nil {
		return Summary{}, err
	}
	s := summarize("sync", opts.Scope, in, imports, r.Manifest.Refs)
	r.Runner.Log.Info("sync inventory", "scope", opts.Scope, "files", s.Files, "discovered", s.Discovered, "imported", s.Imported, "pending", s.Pending, "nontrunk", s.Nontrunk, "unimported", s.Unimported)
	lastProgress := time.Now()
	pending := slices.Clone(in.Ordered)
	for len(pending) > 0 {
		advanced := false
		for i, rev := range pending {
			if imports[key(rev.Path, rev.Number)] != nil || (opts.Scope == "trunk" && rev.Line != "trunk") {
				pending = slices.Delete(pending, i, i+1)
				advanced = true
				break
			}
			if rev.Predecessor != "" && imports[key(rev.Path, rev.Predecessor)] == nil {
				continue
			}
			ready, err := r.prepareBranch(ctx, rev.Ref, in, imports, &opts)
			if err != nil {
				return Summary{}, err
			}
			if !ready {
				continue
			}
			c, err := r.importRevision(ctx, rev, in.Files[rev.Path], &opts)
			if err != nil {
				return Summary{}, diag.New(diag.Code(err), "sync.revision", "revision import stopped; rerun sync after resolving the cause", err, "path_hex", hexBytes(rev.Path), "revision", rev.Number, "ref", rev.Ref, "published_this_run", s.Published)
			}
			imports[key(rev.Path, rev.Number)] = c
			s.Published++
			if opts.ProgressInterval > 0 && time.Since(lastProgress) >= opts.ProgressInterval {
				r.Runner.Log.Info("sync progress", "published", s.Published, "pending", s.Pending-s.Published, "path_hex", hexBytes(rev.Path), "revision", rev.Number, "ref", rev.Ref)
				lastProgress = time.Now()
			}
			pending = slices.Delete(pending, i, i+1)
			advanced = true
			break
		}
		if !advanced {
			return Summary{}, diag.New("integrity", "sync.order", "unresolvable ancestry dependencies", nil, "pending", len(pending))
		}
	}
	if opts.Scope == "all" {
		if err := r.seedRemaining(ctx, in, imports, &opts); err != nil {
			return Summary{}, err
		}
	}
	final := summarize("sync", opts.Scope, in, imports, r.Manifest.Refs)
	final.Published = s.Published
	return final, nil
}

func (r *Repository) importRevision(ctx context.Context, rev *Revision, file *File, opts *Options) (*Imported, error) {
	blob := "-"
	if rev.State != "dead" {
		if err := opts.hit("fetch-start"); err != nil {
			return nil, err
		}
		data, err := r.fetch(ctx, rev)
		if err != nil {
			return nil, err
		}
		if err := opts.hit("fetch-complete"); err != nil {
			return nil, err
		}
		blob, err = r.oid(ctx, data, nil, "hash-object", "-w", "--stdin")
		if err != nil {
			return nil, err
		}
		if err := opts.hit("blob-written"); err != nil {
			return nil, err
		}
	}
	parent := r.Manifest.Refs[rev.Ref]
	tree, err := r.changeTree(ctx, parent, rev.Path, blob)
	if err != nil {
		return nil, err
	}
	if err := opts.hit("tree-written"); err != nil {
		return nil, err
	}
	m := metadata(rev, file, blob)
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	oid, err := r.oid(ctx, message(r.Config.SourceID, "revision", m, rev.Log), exporterEnv(rev.Time, authorDisplay(rev.Author)), args...)
	if err != nil {
		return nil, err
	}
	c := &Imported{OID: oid, Tree: tree, Parent: parent, Kind: "revision", Meta: m, Log: rev.Log}
	if err := opts.hit("commit-written"); err != nil {
		return nil, err
	}
	if err := r.checkTransition(ctx, c); err != nil {
		return nil, err
	}
	if err := opts.hit("before-publish"); err != nil {
		return nil, err
	}
	if rev.Ref != trunkRef {
		r.Manifest.Members[rev.Ref][hexBytes(rev.Path)] = rev.Line[:strings.LastIndexByte(rev.Line, '.')]
	}
	if err := r.publish(ctx, rev.Ref, oid, parent); err != nil {
		return nil, err
	}
	if err := opts.hit("after-publish"); err != nil {
		return nil, err
	}
	r.Runner.Log.Debug("revision published", "path_hex", hexBytes(rev.Path), "revision", rev.Number, "ref", rev.Ref, "commit", oid, "blob", blob, "tree", tree, "parent", parent, "state", rev.State)
	return c, nil
}

// hit is an explicit developer fault hook, never enabled by default. An ordinal
// allows testing interruptions after a nonempty prefix of successful imports.
func (o *Options) hit(stage string) error {
	if o.Failpoint == "" {
		return nil
	}
	if o.faultCounts == nil {
		o.faultCounts = map[string]int{}
	}
	o.faultCounts[stage]++
	spec := o.Failpoint
	kill := strings.HasPrefix(spec, "kill:")
	spec = strings.TrimPrefix(spec, "kill:")
	name, ordinal, hasOrdinal := strings.Cut(spec, ":")
	n := 1
	if hasOrdinal {
		parsed, err := strconv.Atoi(ordinal)
		if err != nil || parsed < 1 {
			return diag.New("usage", "fault.configure", "invalid failpoint ordinal", err)
		}
		n = parsed
	}
	if stage != name || o.faultCounts[stage] != n {
		return nil
	}
	if kill {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return diag.New("injected", "fault.inject", "intentional interruption; rerun without --failpoint", nil, "stage", stage, "ordinal", n)
}
