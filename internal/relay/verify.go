package relay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"cvsgitrelay/internal/diag"
)

func (r *Repository) Verify(ctx context.Context, opts Options) (Summary, error) {
	in, imports, _, err := r.inspect(ctx)
	if err != nil {
		return Summary{}, err
	}
	s := summarize("verify", opts.Scope, in, imports, r.Manifest.Refs)
	if s.Pending != 0 {
		return s, diag.New("integrity", "verify.metadata", "requested scope has missing revision identities; run sync", nil, "scope", opts.Scope, "pending", s.Pending, "unimported", s.Unimported)
	}
	if opts.Scope == "all" {
		for ref := range in.Branches {
			if r.Manifest.Refs[ref] == "" {
				return s, diag.New("integrity", "verify.metadata", "branch has no published seed; run sync", nil, "ref", ref)
			}
		}
	}
	if opts.Mode == "deep" {
		for _, rev := range in.Ordered {
			if opts.Scope == "trunk" && rev.Line != "trunk" {
				continue
			}
			c := imports[key(rev.Path, rev.Number)]
			if rev.State == "dead" {
				continue
			}
			data, err := r.fetch(ctx, rev)
			if err != nil {
				return s, err
			}
			oid, err := r.oid(ctx, data, nil, "hash-object", "--stdin")
			if err != nil {
				return s, err
			}
			if oid != c.Meta.Blob {
				return s, diag.New("integrity", "verify.deep", "source bytes differ from imported blob", nil, "path_hex", hexBytes(rev.Path), "revision", rev.Number, "commit", c.OID, "expected_blob", c.Meta.Blob, "actual_blob", oid)
			}
		}
		if _, err := r.git(ctx, nil, nil, "fsck", "--full", "--no-dangling"); err != nil {
			return s, diag.New("integrity", "verify.objects", "Git object integrity check failed", err)
		}
	}
	if opts.Mode == "tip" {
		if err := r.verifyTip(ctx, in, opts.Scope); err != nil {
			return s, err
		}
		// CVS has no global snapshot. A stable second inventory is necessary
		// before claiming the independent checkout matched this inventory.
		after, err := r.scan(ctx)
		if err != nil {
			return s, err
		}
		if inventoryFingerprint(in) != inventoryFingerprint(after) {
			return s, diag.New("integrity", "verify.tip", "source changed during verification; retry when source is stable", nil)
		}
	}
	return s, nil
}

func inventoryFingerprint(in *Inventory) string {
	var b strings.Builder
	paths := make([]string, 0, len(in.Files))
	for path := range in.Files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		f := in.Files[path]
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00", path, f.Head, f.DefaultBranch)
		names := make([]string, 0, len(f.Symbols))
		for name := range f.Symbols {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			fmt.Fprintf(&b, "%s\x00%s\x00", name, f.Symbols[name])
		}
		b.WriteByte('\n')
	}
	for _, r := range in.Ordered {
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00", r.Path, r.Number, r.Author, r.Time, r.State, r.Ref, r.CommitID, digest(r.Log))
	}
	return digest([]byte(b.String()))
}

func (r *Repository) verifyTip(ctx context.Context, in *Inventory, scope string) error {
	// CVS HEAD may follow a file's default vendor branch. Numeric major 1
	// selects the 1.N trunk line independently of default-branch settings.
	if err := r.compareCheckout(ctx, trunkRef, "1"); err != nil {
		return err
	}
	if scope == "trunk" {
		return nil
	}
	for ref := range in.Branches {
		if strings.HasPrefix(ref, "refs/cvsgitrelay/unmapped/") {
			continue
		}
		name, err := decodeHex(strings.TrimPrefix(ref, "refs/heads/cvs/branches/"))
		if err != nil {
			return err
		}
		if err := r.compareCheckout(ctx, ref, string(name)); err != nil {
			return err
		}
	}
	return nil
}

// An independent CVS working checkout is intentional here: computing the
// expected tree with the importer's own branch model would hide model bugs.
func (r *Repository) compareCheckout(ctx context.Context, ref, symbol string) error {
	dir, err := os.MkdirTemp("", "cvsgitrelay-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := r.cvs(ctx, dir, "checkout", "-n", "-kb", "-r", symbol, "-d", "tree", r.Config.Module); err != nil {
		return err
	}
	want := map[string]string{}
	base := filepath.Join(dir, "tree")
	if err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "CVS" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return diag.New("unsupported", "verify.checkout", "CVS produced a nonregular file", nil)
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if err := validatePath(rel); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > int64(r.Runner.MaxOutput) {
			return diag.New("limit", "verify.checkout", "checkout file exceeds --max-output", nil, "path_hex", hexBytes(rel), "bytes", info.Size())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		oid, err := r.oid(ctx, data, nil, "hash-object", "--stdin")
		if err != nil {
			return err
		}
		want[rel] = oid
		return nil
	}); err != nil {
		return err
	}
	actual := map[string]treeEntry{}
	if tip := r.Manifest.Refs[ref]; tip != "" {
		actual, err = r.treeEntries(ctx, tip)
		if err != nil {
			return err
		}
	}
	if len(want) != len(actual) {
		return diag.New("integrity", "verify.tip", "independent CVS checkout has different file membership", nil, "ref", ref, "cvs_files", len(want), "git_files", len(actual))
	}
	for path, oid := range want {
		if actual[path].OID != oid {
			return diag.New("integrity", "verify.tip", "independent CVS checkout bytes differ", nil, "ref", ref, "path_hex", hexBytes(path), "cvs_blob", oid, "git_blob", actual[path].OID)
		}
	}
	return nil
}
