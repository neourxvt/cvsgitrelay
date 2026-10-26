package relay

import (
	"context"
	"slices"
	"strings"

	"cvsgitrelay/internal/diag"
)

// Mirror identities use physical source paths, avoiding collisions between a
// module's checkout names and the repository-wide administrative directory.
func buildMirrorInventory(files []*File) (*Inventory, error) {
	in := &Inventory{Files: map[string]*File{}, Revisions: map[string]*Revision{}}
	for _, file := range files {
		file.Path = file.SourcePath
		if _, exists := in.Files[file.Path]; exists {
			return nil, diag.New("source", "mirror.inventory", "duplicate physical archive", nil)
		}
		in.Files[file.Path] = file
		if len(file.Revisions) == 0 && file.Head != "" {
			return nil, diag.New("source", "mirror.inventory", "archive head has no accessible revision", nil)
		}
		slices.Sort(file.Access)
		lines := map[string][]*Revision{}
		for _, rev := range file.Revisions {
			n, err := numbers(rev.Number)
			if err != nil || len(n)%2 != 0 || slices.Contains(n, 0) {
				return nil, diag.New("source", "mirror.inventory", "invalid CVS revision number", err)
			}
			rev.Path, rev.SourcePath = file.Path, file.SourcePath
			rev.Line = rev.Number[:strings.LastIndexByte(rev.Number, '.')]
			if len(n) == 2 {
				rev.Line = "trunk"
				in.Trunk++
			}
			if rev.State == "dead" {
				in.Dead++
			}
			if strings.HasPrefix(rev.Number, "1.1.1.") {
				in.Vendor++
			}
			if in.Revisions[key(rev.Path, rev.Number)] != nil {
				return nil, diag.New("source", "mirror.inventory", "duplicate revision", nil)
			}
			in.Revisions[key(rev.Path, rev.Number)] = rev
			lines[rev.Line] = append(lines[rev.Line], rev)
			slices.SortFunc(rev.Branches, compareNumber)
		}
		if len(file.Revisions) > 0 && len(lines["trunk"]) == 0 {
			return nil, diag.New("source", "mirror.inventory", "archive has no trunk revisions", nil)
		}
		for line, revisions := range lines {
			slices.SortFunc(revisions, func(a, b *Revision) int { return compareNumber(a.Number, b.Number) })
			ordinal := 0
			for i, rev := range revisions {
				n, _ := numbers(rev.Number)
				if n[len(n)-1]-ordinal > 1 {
					in.NumberingGaps++
				}
				ordinal = n[len(n)-1]
				if i > 0 {
					rev.Predecessor = revisions[i-1].Number
				} else if line != "trunk" {
					rev.Predecessor = line[:strings.LastIndexByte(line, '.')]
				}
			}
			if line == "trunk" && file.Head != revisions[len(revisions)-1].Number {
				return nil, diag.New("source", "mirror.inventory", "archive head does not match trunk", nil)
			}
		}
	}
	for _, rev := range in.Revisions {
		if rev.Predecessor != "" && in.Revisions[key(rev.Path, rev.Predecessor)] == nil {
			return nil, diag.New("source", "mirror.inventory", "missing revision predecessor", nil)
		}
		in.Ordered = append(in.Ordered, rev)
	}
	slices.SortFunc(in.Ordered, func(a, b *Revision) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return compareNumber(a.Number, b.Number)
	})
	return in, nil
}

// Read administration through CVS too, retaining every readable versioned
// administrative archive without applying its contents as local runtime policy.
func (r *Repository) mirrorScan(ctx context.Context, progress *mirrorProgress) (*Inventory, error) {
	in, err := r.scan(ctx)
	if err != nil {
		return nil, err
	}
	root, _ := sourceDirectory(r.Config.CVSRoot)
	progress.update("activity", "Reading CVS administration history", "path", "CVSROOT/")
	r.Runner.Log.Info("Reading CVS administration history", "path", "CVSROOT/")
	data, err := r.cvs(ctx, "", "rlog", "CVSROOT/")
	if err != nil {
		return nil, err
	}
	admin, err := parseMappedRlogMode(data, []rlogMapping{{RCSPrefix: root + "/CVSROOT/", SourcePrefix: "CVSROOT"}}, true)
	if err != nil {
		return nil, err
	}
	var files []*File
	for _, file := range in.Files {
		files = append(files, file)
	}
	for _, file := range admin.Files {
		if in.Files[file.SourcePath] == nil {
			files = append(files, file)
		}
	}
	combined, err := buildMirrorInventory(files)
	if err != nil {
		return nil, err
	}
	combined.Layout, combined.Submodules = in.Layout, in.Submodules
	return combined, nil
}
