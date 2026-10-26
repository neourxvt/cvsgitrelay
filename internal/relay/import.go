package relay

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

type Options struct {
	Scope            string
	Connections      int
	Mode             string
	Failpoint        string
	ProgressInterval time.Duration
	faultCounts      map[string]int
}

// Summaries identify the configured integrity guarantee on every operation.
type Summary struct {
	Operation      string `json:"operation"`
	Verification   string `json:"verification_policy"`
	State          string `json:"state"`
	Files          int    `json:"files"`
	Discovered     int    `json:"discovered"`
	Imported       int    `json:"imported"`
	Pending        int    `json:"pending"`
	Published      int    `json:"published"`
	Refs           int    `json:"refs"`
	SourceBytes    int64  `json:"source_bytes"`
	SourceRequests int    `json:"source_requests"`
	ObjectWrites   int    `json:"object_writes"`
	ElapsedMillis  int64  `json:"elapsed_ms"`
}

func (r *Repository) generationSummary(op string, in canonical.Inventory) Summary {
	s := Summary{Operation: op, Verification: r.Config.Policy.Verify, State: r.StateOID, Discovered: len(in.Records), Refs: len(r.Manifest.Refs)}
	files := map[string]bool{}
	for _, rev := range in.Records {
		files[rev.Key.Archive] = true
		if _, ok := r.Manifest.Revisions[rev.Key.ID()]; ok {
			s.Imported++
		} else {
			s.Pending++
		}
	}
	s.Files = len(files)
	return s
}

// Sync verifies the entire fixed scope before deriving a fresh canonical graph.
// Polling history influences neither ordering nor branch membership.
func (r *Repository) Sync(ctx context.Context, opts Options) (Summary, error) {
	start := time.Now()
	r.objectWrites = 0
	if opts.Mode != "" && opts.Mode != r.Config.Policy.Verify {
		return Summary{}, diag.New("config", "sync.policy", "verification mode differs from pinned canonical policy", nil)
	}
	if opts.Scope != "" && opts.Scope != "all" {
		return Summary{}, diag.New("usage", "sync.scope", "partial scopes cannot publish v2 generations", nil)
	}
	if opts.Connections < 0 {
		return Summary{}, diag.New("usage", "sync.configure", "connections must be positive", nil)
	}
	if err := r.recoverPublication(ctx); err != nil {
		return Summary{}, err
	}
	if err := r.loadCurrent(ctx); err != nil {
		return Summary{}, err
	}
	in, stats, err := r.acceptSource(ctx, &opts)
	if err != nil {
		return Summary{}, err
	}
	g, err := canonical.Build(in, r.Config.Policy)
	if err != nil {
		return Summary{}, diag.New("source", "canonical.build", "source cannot form an unambiguous canonical graph", err)
	}
	s := r.generationSummary("sync", in)
	previous := r.StateOID
	if err := r.publishGeneration(ctx, g, &opts); err != nil {
		return Summary{}, err
	}
	if err := r.cacheAccepted(in); err != nil && r.Runner.Log != nil {
		r.Runner.Log.Warn("Content cache could not be saved; the next run will re-read source bytes")
	}
	s.Imported = len(in.Records)
	s.Pending = 0
	s.State = r.StateOID
	s.Refs = len(g.Manifest.Refs)
	if previous != g.State {
		s.Published = len(in.Records)
	}
	s.SourceBytes = stats.Bytes
	s.SourceRequests = stats.Requests
	s.ObjectWrites = r.objectWrites
	s.ElapsedMillis = time.Since(start).Milliseconds()
	return s, nil
}

func (r *Repository) Status(ctx context.Context, scope string) (Summary, error) {
	if scope != "" && scope != "all" {
		return Summary{}, diag.New("usage", "status.scope", "v2 supports only the complete configured scope", nil)
	}
	if err := r.loadCurrent(ctx); err != nil {
		return Summary{}, err
	}
	in, stats, err := r.acceptSource(ctx, &Options{})
	if err != nil {
		return Summary{}, err
	}
	s := r.generationSummary("status", in)
	s.SourceBytes = stats.Bytes
	s.SourceRequests = stats.Requests
	return s, nil
}

func (r *Repository) Verify(ctx context.Context, opts Options) (Summary, error) {
	if opts.Mode != "" && opts.Mode != r.Config.Policy.Verify {
		return Summary{}, diag.New("config", "verify.policy", "verification mode differs from pinned canonical policy", nil)
	}
	if opts.Scope != "" && opts.Scope != "all" {
		return Summary{}, diag.New("usage", "verify.scope", "v2 supports only the complete configured scope", nil)
	}
	if err := r.loadCurrent(ctx); err != nil {
		return Summary{}, err
	}
	in, stats, err := r.acceptSource(ctx, &opts)
	if err != nil {
		return Summary{}, err
	}
	g, err := canonical.Build(in, r.Config.Policy)
	if err != nil {
		return Summary{}, err
	}
	s := r.generationSummary("verify", in)
	s.SourceBytes = stats.Bytes
	s.SourceRequests = stats.Requests
	if g.State != r.StateOID {
		return s, diag.New("integrity", "verify.generation", "source has a different canonical generation; run sync", nil)
	}
	// Validate every expected raw object without rewriting objects or refs.
	for oid, obj := range g.Objects {
		data, err := r.git(ctx, nil, nil, "cat-file", obj.Kind, oid)
		if err != nil {
			return s, err
		}
		if canonical.OID(obj.Kind, data) != oid {
			return s, diag.New("integrity", "verify.objects", "canonical object is corrupt", nil)
		}
	}
	return s, nil
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
