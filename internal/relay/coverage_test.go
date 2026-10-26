package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"

	"cvsgitrelay/internal/canonical"
)

func TestV1ToNewDestinationCoverage(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	next := openFixture(t, root, "full")
	if _, err := next.Sync(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	newPath := next.Path
	newState := next.StateOID
	next.Close()
	oldPath := filepath.Join(t.TempDir(), "v1.git")
	runner := testRunner()
	if _, err := runner.Run(ctx, "", nil, nil, "git", "init", "--bare", oldPath); err != nil {
		t.Fatal(err)
	}
	old := &Repository{Path: oldPath, Runner: runner}
	o := canonical.Objects{}
	blob := o.Put("blob", []byte("initial\n"))
	tree, err := o.Tree(map[string]string{"active": blob})
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"path_hex": hexBytes("active"), "revision": "1.1", "state": "Exp", "blob": blob}
	meta, _ := json.Marshal(metadata)
	message := "cvsgitrelay-Version: 1\ncvsgitrelay-Source-ID: old-random-id\ncvsgitrelay-Kind: revision\ncvsgitrelay-Data: " + base64.StdEncoding.EncodeToString(meta) + "\n\nold log\n"
	event := o.Synthetic(tree, nil, message)
	manifest, _ := json.Marshal(map[string]any{"version": 1, "refs": map[string]string{trunkRef: event}})
	manifestBlob := o.Put("blob", manifest)
	stateTree, _ := o.Tree(map[string]string{"manifest.json": manifestBlob})
	state := o.Synthetic(stateTree, nil, "cvsgitrelay checkpoint v1\n")
	if _, err := old.storeObjects(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	if err := old.cas(ctx, trunkRef, "", event); err != nil {
		t.Fatal(err)
	}
	if err := old.cas(ctx, stateRef, "", state); err != nil {
		t.Fatal(err)
	}
	if err := durableFile(filepath.Join(oldPath, "cvsgitrelay.json"), []byte(`{"format_version":1,"source_id":"old-random-id"}`)); err != nil {
		t.Fatal(err)
	}
	s, err := CompareCoverage(ctx, testRunner(), oldPath, newPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.PreviousRevisions != 1 || s.CurrentRevisions != 9 || len(s.Missing) != 0 || len(s.ContentChanged) != 0 {
		t.Fatalf("wrong coverage %+v", s)
	}
	refs, err := old.allRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if refs[stateRef] != state || refs[trunkRef] != event {
		t.Fatal("comparison rewrote v1 refs")
	}
	check, err := Open(ctx, testRunner(), newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if check.StateOID != newState {
		t.Fatal("comparison rewrote v2 state")
	}
}
