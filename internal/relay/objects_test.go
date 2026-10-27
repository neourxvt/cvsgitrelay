package relay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

// Batched object I/O needs only Git, not CVS or reftable: store a generation,
// validate it like a reopened state, repack it, and detect a damaged payload.
func TestBatchedObjectStore(t *testing.T) {
	ctx := context.Background()
	runner := testRunner()
	// A small output budget forces several cat-file batches.
	runner.MaxOutput = 32 << 10
	path := filepath.Join(t.TempDir(), "objects.git")
	if _, err := runner.Run(ctx, "", nil, nil, "git", "init", "--bare", "--quiet", path); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	// Conversions must never apply to canonical blobs.
	if _, err := runner.Run(ctx, "", nil, nil, "git", "--git-dir="+path, "config", "core.autocrlf", "true"); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	policy := canonical.Policy{Source: "test-source", Module: "module", Submodules: "include", Verify: "full", Mappings: []canonical.Mapping{{Archive: "module"}}}
	r := &Repository{Path: path, Runner: runner, Config: Config{SourceID: policy.Source, Module: policy.Module, Policy: policy}}
	var in canonical.Inventory
	for i := 0; i < 40; i++ {
		file := fmt.Sprintf("d%d/f%d", i%3, i)
		content := []byte(strings.Repeat(fmt.Sprintf("line %d\r\n", i), 50))
		in.Records = append(in.Records, canonical.Record{Key: canonical.Key{Source: policy.Source, Archive: "module/" + file, Number: "1.1"}, Path: file, Time: int64(i), Author: "author", Log: []byte("log\n"), State: "live", Present: true, Content: content})
	}
	g, err := canonical.Build(in, policy)
	if err != nil {
		t.Fatal(err)
	}
	written, err := r.storeObjects(ctx, g.Objects, &Options{})
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if written != len(g.Objects) {
		t.Fatalf("wrote %d of %d objects", written, len(g.Objects))
	}
	if _, err := r.readState(ctx, g.State); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if _, err := r.git(ctx, nil, nil, "update-ref", stateRef, g.State); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if err := r.packObjects(ctx); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if err := r.compareObjects(ctx, g.Objects); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	// Existing packed objects are verified again but not rewritten.
	if written, err := r.storeObjects(ctx, g.Objects, nil); err != nil || written != 0 {
		t.Fatalf("rewrote %d objects: %v", written, err)
	}
	staged, _ := filepath.Glob(filepath.Join(path, objectStagingPrefix+"*"))
	if len(staged) != 0 {
		t.Fatalf("staging directories left behind: %v", staged)
	}
	// A different expected payload under a stored ID must be reported.
	damaged := canonical.Objects{}
	for oid, o := range g.Objects {
		if o.Kind == "blob" {
			damaged[oid] = canonical.Object{Kind: o.Kind, Data: append([]byte("x"), o.Data...)}
			break
		}
	}
	if err := r.compareObjects(ctx, damaged); err == nil {
		t.Fatal("payload mismatch accepted")
	}
	if err := r.readObjects(ctx, []string{strings.Repeat("1", 40)}, func(string, string, []byte) error { return nil }); diag.Code(err) != "integrity" {
		t.Fatalf("missing object accepted: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(path, "objects", "pack")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.readState(ctx, g.State); err == nil {
		t.Fatal("state without objects accepted")
	}
}

// Format 3 events must pass the reopened-state verifier and git fsck --strict
// for every kind of log, and show readable authors and logs in git log.
func TestFormat3EventsVerify(t *testing.T) {
	ctx := context.Background()
	runner := testRunner()
	path := filepath.Join(t.TempDir(), "objects.git")
	if _, err := runner.Run(ctx, "", nil, nil, "git", "init", "--bare", "--quiet", path); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	policy := canonical.Policy{Source: "test-source", Module: "module", Submodules: "include", Verify: "full", Mappings: []canonical.Mapping{{Archive: "module"}},
		Format: 3, Authors: map[string]string{"root": "Root User <root@example.com>"}}
	r := &Repository{Path: path, Runner: runner, Config: Config{Version: 3, SourceID: policy.Source, Module: policy.Module, Policy: policy}}
	logs := []string{"Fix the build\n\nDetails.\n", "*** empty log message ***\n", "", "no newline", "bad \xff byte\n", "nul\x00\n"}
	authors := []string{"root", "alice", "odd user"}
	var in canonical.Inventory
	for i, log := range logs {
		in.Records = append(in.Records, canonical.Record{Key: canonical.Key{Source: policy.Source, Archive: "module/f", Number: fmt.Sprintf("1.%d", i+1)}, Path: "f", Time: int64(i), Author: authors[i%len(authors)], Log: []byte(log), State: "live", Present: true, Content: []byte(fmt.Sprint(i))})
		if i > 0 {
			in.Records[i].Parent = &canonical.Key{Source: policy.Source, Archive: "module/f", Number: fmt.Sprintf("1.%d", i)}
		}
	}
	g, err := canonical.Build(in, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.storeObjects(ctx, g.Objects, nil); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if _, err := r.readState(ctx, g.State); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	// A format 2 reader must reject the same events.
	v2 := *r
	v2.Config.Version, v2.Config.Policy.Format, v2.Config.Policy.Authors = 2, 0, nil
	if _, err := v2.readState(ctx, g.State); err == nil {
		t.Fatal("format 2 verifier accepted format 3 events")
	}
	tip := g.Manifest.Refs[trunkRef]
	out, err := r.git(ctx, nil, nil, "log", "--reverse", "--format=%an <%ae>|%s", tip)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	// Each line starts from its synthetic root commit, which is unchanged.
	want := "CVSGITRELAY <relay@invalid>|cvsgitrelay-root-v2 line=7472756e6b\nRoot User <root@example.com>|Fix the build\nalice <alice@cvs.invalid>|*** empty log message ***\nCVS-6f64642075736572 <cvs@invalid>|" + strings.TrimSuffix(canonical.EmptyLogText, "\n") +
		"\nRoot User <root@example.com>|no newline\nalice <alice@cvs.invalid>|bad � byte\nCVS-6f64642075736572 <cvs@invalid>|nul�\n"
	if string(out) != want {
		t.Fatalf("unexpected readable history:\n%s", out)
	}
	// Git shows the whole message: the log plus one provenance line, and a
	// further line only for the mapped author.
	message, err := r.git(ctx, nil, nil, "log", "--reverse", "--format=%B", "--grep=Fix the build", tip)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if want := "Fix the build\n\nDetails.\n\nCVS: 1.1 module/f\nCVS-Author: root\n\n"; string(message) != want {
		t.Fatalf("unexpected message:\n%s", message)
	}
}
