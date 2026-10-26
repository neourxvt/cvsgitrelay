package relay

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"cvsgitrelay/internal/diag"
)

const seedLog = "Synthetic CVS branch seed; per-file forkpoints, not a historical changeset.\n"

func encodedMembers(members map[string]string) map[string]string {
	out := map[string]string{}
	for path, rev := range members {
		out[hexBytes(path)] = rev
	}
	return out
}

// prepareBranch waits for every seed forkpoint's canonical import identity.
// Forkpoint content initializes a checkout tree without importing it twice.
func (r *Repository) prepareBranch(ctx context.Context, ref string, in *Inventory, imports map[string]*Imported, opts *Options) (bool, error) {
	if ref == trunkRef || r.Manifest.Refs[ref] != "" {
		return true, nil
	}
	members := in.Branches[ref]
	if members == nil {
		return false, diag.New("integrity", "branch.seed", "branch has no membership model", nil, "ref", ref)
	}
	entries := map[string]string{}
	for path, fork := range members {
		c := imports[key(path, fork)]
		if c == nil {
			return false, nil
		}
		if c.Meta.Blob != "-" {
			entries[path] = c.Meta.Blob
		}
	}
	tree, err := r.seedTree(ctx, entries)
	if err != nil {
		return false, err
	}
	m := Metadata{Ref: ref, Members: encodedMembers(members)}
	oid, err := r.oid(ctx, message(r.Config.SourceID, "seed", m, []byte(seedLog)), exporterEnv(0, "cvsgitrelay"), "commit-tree", tree)
	if err != nil {
		return false, err
	}
	if err := opts.hit("seed-written"); err != nil {
		return false, err
	}
	r.Manifest.Members[ref] = m.Members
	if err := r.publish(ctx, ref, oid, ""); err != nil {
		return false, err
	}
	if err := opts.hit("seed-published"); err != nil {
		return false, err
	}
	r.Runner.Log.Debug("branch seed published", "ref", ref, "commit", oid, "members", len(members), "live_files", len(entries))
	return true, nil
}

func (r *Repository) validateBranches(ctx context.Context, in *Inventory, imports map[string]*Imported, chains map[string][]*Imported) error {
	for ref, chain := range chains {
		if ref == trunkRef {
			continue
		}
		if len(chain) == 0 || chain[len(chain)-1].Kind != "seed" {
			return diag.New("integrity", "branch.validate", "nontrunk ref has no synthetic seed", nil, "ref", ref)
		}
		seed := chain[len(chain)-1]
		m := Metadata{Ref: ref, Members: seed.Meta.Members}
		a, _ := json.Marshal(m)
		b, _ := json.Marshal(seed.Meta)
		if string(a) != string(b) || string(seed.Log) != seedLog || len(seed.Meta.Members) == 0 {
			return diag.New("integrity", "branch.validate", "noncanonical seed metadata", nil, "ref", ref, "commit", seed.OID)
		}
		entries, err := r.treeEntries(ctx, seed.Tree)
		if err != nil {
			return err
		}
		for pathHex, rev := range seed.Meta.Members {
			path, err := decodePath(pathHex)
			if err != nil {
				return err
			}
			c := imports[key(path, rev)]
			if c == nil || in.Branches[ref][path] != rev || r.Manifest.Members[ref][pathHex] != rev {
				return diag.New("integrity", "branch.validate", "seed forkpoint disappeared or branch symbol moved", nil, "ref", ref, "path_hex", pathHex, "forkpoint", rev)
			}
			e, present := entries[path]
			if (c.Meta.Blob == "-" && present) || (c.Meta.Blob != "-" && (!present || e.OID != c.Meta.Blob)) {
				return diag.New("integrity", "branch.validate", "branch seed tree differs from canonical forkpoint", nil, "ref", ref, "path_hex", pathHex, "commit", seed.OID)
			}
			delete(entries, path)
		}
		if len(entries) != 0 {
			return diag.New("integrity", "branch.validate", "seed contains unaccounted files", nil, "ref", ref, "commit", seed.OID)
		}
		for pathHex, fork := range r.Manifest.Members[ref] {
			path, err := decodePath(pathHex)
			if err != nil {
				return err
			}
			if in.Branches[ref][path] != fork {
				return diag.New("integrity", "branch.validate", "published branch membership disappeared or moved", nil, "ref", ref, "path_hex", pathHex, "forkpoint", fork)
			}
			if seed.Meta.Members[pathHex] == fork {
				continue
			}
			// The only supported later membership change starts from an absent
			// forkpoint and a real revision on this branch. Live retroactive
			// additions would require a separate synthetic transition policy.
			base := in.Revisions[key(path, fork)]
			if base == nil || base.State != "dead" {
				return diag.New("unsupported", "branch.membership", "late live forkpoint requires unsupported retroactive membership transition", nil, "ref", ref, "path_hex", pathHex, "forkpoint", fork)
			}
			found := false
			for _, c := range chain {
				if c.Kind == "revision" && c.Meta.PathHex == pathHex {
					found = true
					break
				}
			}
			if !found {
				return diag.New("integrity", "branch.membership", "late membership has no published branch revision", nil, "ref", ref, "path_hex", pathHex)
			}
		}
		for path, fork := range in.Branches[ref] {
			if r.Manifest.Members[ref][hexBytes(path)] != "" {
				continue
			}
			base := in.Revisions[key(path, fork)]
			if base == nil || base.State != "dead" {
				return diag.New("unsupported", "branch.membership", "new symbol introduces a live forkpoint into an existing branch; an explicit membership transition is required", nil, "ref", ref, "path_hex", hexBytes(path), "forkpoint", fork)
			}
		}
	}
	return nil
}

func (r *Repository) seedRemaining(ctx context.Context, in *Inventory, imports map[string]*Imported, opts *Options) error {
	refs := make([]string, 0, len(in.Branches))
	for ref := range in.Branches {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		ready, err := r.prepareBranch(ctx, ref, in, imports, opts)
		if err != nil {
			return err
		}
		if !ready {
			return diag.New("integrity", "branch.seed", "unresolved branch seed dependencies", nil, "ref", ref)
		}
	}
	return nil
}

func branchKind(ref string) string {
	if strings.HasPrefix(ref, "refs/cvsgitrelay/unmapped/") {
		return "archive"
	}
	return "named"
}
