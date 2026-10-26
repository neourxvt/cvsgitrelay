package relay

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"cvsgitrelay/internal/diag"
)

// changeTree never uses the destination's default index or a Git working tree.
// NUL framing preserves spaces and all other accepted POSIX filename bytes.
func (r *Repository) changeTree(ctx context.Context, parent, path, blob string) (string, error) {
	return r.writeTree(ctx, parent, map[string]string{path: blob})
}

func (r *Repository) seedTree(ctx context.Context, entries map[string]string) (string, error) {
	return r.writeTree(ctx, "", entries)
}

func (r *Repository) writeTree(ctx context.Context, parent string, entries map[string]string) (string, error) {
	dir, err := os.MkdirTemp(r.Path, "cvsgitrelay-tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	args := []string{"read-tree", "--empty"}
	if parent != "" {
		args = []string{"read-tree", parent + "^{tree}"}
	}
	if _, err := r.git(ctx, nil, env, args...); err != nil {
		return "", err
	}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	var records strings.Builder
	for _, path := range paths {
		blob := entries[path]
		if blob == "-" {
			records.WriteString("0 " + zeroOID + "\t" + path + "\x00")
		} else {
			records.WriteString("100644 " + blob + "\t" + path + "\x00")
		}
	}
	if _, err := r.git(ctx, []byte(records.String()), env, "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := r.oid(ctx, nil, env, "write-tree")
	if err != nil {
		return "", err
	}
	// Git can resolve its built-in empty tree without a loose object. Store
	// it explicitly so an empty seed/deletion tree survives fsck and transfer.
	if tree == "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		return r.oid(ctx, nil, nil, "hash-object", "-t", "tree", "-w", "--stdin")
	}
	return tree, nil
}

type treeEntry struct {
	Mode string
	OID  string
}

func (r *Repository) treeEntries(ctx context.Context, tree string) (map[string]treeEntry, error) {
	out, err := r.git(ctx, nil, nil, "ls-tree", "-rz", "--full-tree", tree)
	if err != nil {
		return nil, err
	}
	entries := map[string]treeEntry{}
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		head, path, ok := strings.Cut(entry, "\t")
		fields := strings.Split(head, " ")
		if !ok || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" || !validOID(fields[2]) {
			return nil, diag.New("integrity", "tree.read", "invalid tree entry or file mode", nil, "tree", tree)
		}
		if err := validatePath(path); err != nil {
			return nil, err
		}
		entries[path] = treeEntry{Mode: fields[0], OID: fields[2]}
	}
	return entries, nil
}

func (r *Repository) checkTransition(ctx context.Context, c *Imported) error {
	pathBytes, err := decodePath(c.Meta.PathHex)
	if err != nil {
		return err
	}
	entries, err := r.treeEntries(ctx, c.Tree)
	if err != nil {
		return err
	}
	entry, present := entries[pathBytes]
	if (c.Meta.State == "dead" && (present || c.Meta.Blob != "-")) || (c.Meta.State == "Exp" && (!present || entry.OID != c.Meta.Blob || !validOID(c.Meta.Blob))) {
		return diag.New("integrity", "tree.verify", "revision tree does not contain the recorded blob or deletion", nil, "commit", c.OID, "path_hex", c.Meta.PathHex, "revision", c.Meta.Revision, "expected_blob", c.Meta.Blob)
	}
	before := map[string]treeEntry{}
	if c.Parent != "" {
		before, err = r.treeEntries(ctx, c.Parent)
		if err != nil {
			return err
		}
	}
	delete(before, pathBytes)
	delete(entries, pathBytes)
	if len(before) != len(entries) {
		return diag.New("integrity", "tree.verify", "revision changed unrelated paths", nil, "commit", c.OID)
	}
	for p, e := range entries {
		if before[p] != e {
			return diag.New("integrity", "tree.verify", "revision changed an unrelated file", nil, "commit", c.OID, "path_hex", hexBytes(p))
		}
	}
	return nil
}

func decodePath(value string) (string, error) {
	b, err := decodeHex(value)
	if err != nil {
		return "", err
	}
	p := string(b)
	if err := validatePath(p); err != nil {
		return "", err
	}
	return p, nil
}

func decodeHex(value string) ([]byte, error) {
	b, err := hex.DecodeString(value)
	if value == "" || err != nil || hexBytes(string(b)) != value {
		return nil, diag.New("integrity", "provenance.decode", "invalid canonical hex encoding", err)
	}
	return b, nil
}
