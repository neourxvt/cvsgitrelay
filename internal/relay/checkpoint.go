package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"cvsgitrelay/internal/diag"
)

func (r *Repository) readCheckpoint(ctx context.Context, oid string) (string, Manifest, error) {
	var m Manifest
	data, err := r.git(ctx, nil, nil, "cat-file", "commit", oid)
	if err != nil {
		return "", m, err
	}
	header, body, ok := bytes.Cut(data, []byte("\n\n"))
	if !ok || string(body) != "cvsgitrelay checkpoint v1\n" {
		return "", m, diag.New("integrity", "checkpoint.read", "invalid checkpoint commit", nil, "commit", oid)
	}
	var parent, tree string
	lines := strings.Split(string(header), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "tree ") {
		return "", m, diag.New("integrity", "checkpoint.read", "invalid checkpoint headers", nil, "commit", oid)
	}
	tree = strings.TrimPrefix(lines[0], "tree ")
	lines = lines[1:]
	if strings.HasPrefix(lines[0], "parent ") {
		parent = strings.TrimPrefix(lines[0], "parent ")
		lines = lines[1:]
	}
	if !validOID(tree) || (parent != "" && !validOID(parent)) || len(lines) != 2 || lines[0] != "author cvsgitrelay <cvs@cvsgitrelay.invalid> 0 +0000" || lines[1] != "committer cvsgitrelay <exporter@cvsgitrelay.invalid> 0 +0000" {
		return "", m, diag.New("integrity", "checkpoint.read", "uncontrolled checkpoint headers", nil, "commit", oid)
	}
	listing, err := r.git(ctx, nil, nil, "ls-tree", "-z", tree)
	if err != nil {
		return "", m, err
	}
	parts := strings.Split(string(listing), "\x00")
	if len(parts) != 2 || parts[1] != "" || !strings.HasPrefix(parts[0], "100644 blob ") || !strings.HasSuffix(parts[0], "\tmanifest.json") {
		return "", m, diag.New("integrity", "checkpoint.read", "unexpected checkpoint tree", nil, "commit", oid)
	}
	blob := strings.TrimSuffix(strings.TrimPrefix(parts[0], "100644 blob "), "\tmanifest.json")
	if !validOID(blob) {
		return "", m, diag.New("integrity", "checkpoint.read", "invalid manifest object", nil, "commit", oid)
	}
	content, err := r.git(ctx, nil, nil, "cat-file", "blob", blob)
	if err != nil {
		return "", m, err
	}
	if err := decodeJSON(content, &m); err != nil {
		return "", m, diag.New("integrity", "checkpoint.read", "invalid checkpoint manifest", err, "commit", oid)
	}
	canonical, _ := json.Marshal(m)
	if !bytes.Equal(canonical, content) || m.Version != 1 || m.ConfigHash != r.configHash() || m.Refs == nil || m.Members == nil {
		return "", m, diag.New("integrity", "checkpoint.read", "noncanonical manifest or immutable settings changed", nil, "commit", oid)
	}
	for ref, id := range m.Refs {
		if !ownedDataRef(ref) || !validOID(id) {
			return "", m, diag.New("integrity", "checkpoint.read", "invalid managed ref entry", nil, "commit", oid, "ref", ref)
		}
	}
	if len(m.Layout) > 0 {
		if err := validateMappings(m.Layout); err != nil {
			return "", m, err
		}
	}
	return parent, m, nil
}

// Check every manifest transition so isolated checkpoint edits and data-ref
// rewinds are rejected. A coherent rollback of all Git refs requires an external
// backup/audit anchor to detect and is outside local Git's threat model.
func (r *Repository) validateCheckpoints(ctx context.Context) error {
	seen := map[string]bool{}
	var child *Manifest
	for oid := r.StateOID; oid != ""; {
		if seen[oid] {
			return diag.New("integrity", "checkpoint.walk", "checkpoint cycle", nil, "commit", oid)
		}
		seen[oid] = true
		parent, m, err := r.readCheckpoint(ctx, oid)
		if err != nil {
			return err
		}
		if child != nil {
			if (len(m.Layout) > 0 && !sameMappings(m.Layout, child.Layout)) || (len(m.Layout) == 0 && len(m.Refs) > 0 && len(child.Layout) > 0 && !sameMappings(child.Layout, literalMapping(r.Config.Module))) {
				return diag.New("integrity", "checkpoint.walk", "published module layout was changed", nil, "commit", oid)
			}
			changed := ""
			for ref, old := range m.Refs {
				if child.Refs[ref] == "" {
					return diag.New("integrity", "checkpoint.walk", "checkpoint removed a managed ref", nil, "ref", ref, "commit", oid)
				}
				if child.Refs[ref] == old {
					continue
				}
				if changed != "" {
					return diag.New("integrity", "checkpoint.walk", "checkpoint changed multiple data refs", nil)
				}
				changed = ref
			}
			for ref := range child.Refs {
				if m.Refs[ref] == "" {
					if changed != "" {
						return diag.New("integrity", "checkpoint.walk", "checkpoint created multiple data refs", nil)
					}
					changed = ref
				}
			}
			if changed == "" {
				return diag.New("integrity", "checkpoint.walk", "checkpoint has no data publication", nil)
			}
			c, err := r.readCommit(ctx, child.Refs[changed])
			if err != nil {
				return err
			}
			if c.Parent != m.Refs[changed] || c.Meta.Ref != changed {
				return diag.New("integrity", "checkpoint.walk", "data ref publication is not append-only", nil, "ref", changed, "commit", c.OID)
			}
			for ref, members := range m.Members {
				for p, rev := range members {
					if child.Members[ref][p] != rev {
						return diag.New("integrity", "checkpoint.walk", "branch membership was removed or changed", nil, "ref", ref, "path_hex", p)
					}
				}
			}
			for ref, members := range child.Members {
				if child.Refs[ref] == "" {
					return diag.New("integrity", "checkpoint.walk", "membership has no managed ref", nil, "ref", ref)
				}
				for p, rev := range members {
					if m.Members[ref][p] != rev && ref != changed {
						return diag.New("integrity", "checkpoint.walk", "membership changed outside its ref publication", nil, "ref", ref, "path_hex", p)
					}
				}
			}
		}
		if parent == "" && (len(m.Refs) != 0 || len(m.Members) != 0) {
			return diag.New("integrity", "checkpoint.walk", "initial checkpoint must contain no imports", nil)
		}
		copy := m
		child = &copy
		oid = parent
	}
	return nil
}
