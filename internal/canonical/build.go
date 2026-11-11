package canonical

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

type Generation struct {
	State    string
	Manifest Manifest
	Objects  Objects
	// Corrections counts the commits of growing history that bring a line
	// to the current source after published history changed in CVS.
	Corrections int
}
type fileLine struct {
	id, archive, number string
	events              []Record
	fork                *Record
}
type projectLine struct {
	id      string
	aliases []string
	members []*fileLine
	// label names the line in its first commit when its ID differs from the
	// one the source suggests, as for a branch of growing history that was
	// deleted in CVS and created again.
	label string
}

func fileLineID(archive, line string) string { return string(append(frame(archive), frame(line)...)) }
func eventCompare(a, b Record) int {
	if c := cmp.Compare(a.Time, b.Time); c != 0 {
		return c
	}
	if c := strings.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	if c := CompareNumber(a.Key.Number, b.Key.Number); c != 0 {
		return c
	}
	return strings.Compare(a.Key.Archive, b.Key.Archive)
}

// structure is the normalized source arranged into file lines, and into the
// project lines that the current branch symbols form from them.
type structure struct {
	in      Inventory
	records map[string]Record
	flines  map[string]*fileLine
	// projects are the project lines by ID, and assignment maps every file
	// line ID to the ID of its project line.
	projects   map[string]*projectLine
	assignment map[string]string
	// blobs maps the ID of every present record to its blob.
	blobs map[string]string
}

// analyze normalizes the source, checks the ancestry of every file line and
// groups the file lines into project lines. It stores a blob of every present
// revision in objects.
func analyze(input Inventory, p Policy, objects Objects) (*structure, error) {
	in, err := input.Normalize(p)
	if err != nil {
		return nil, err
	}
	if _, named := p.Authors[RelayAuthor]; named && p.Version() == 3 {
		for _, r := range in.Records {
			if r.Author == RelayAuthor {
				return nil, fmt.Errorf("the CVS username %q is reserved for the relay identity in the author map", RelayAuthor)
			}
		}
	}
	records := map[string]Record{}
	flines := map[string]*fileLine{}
	ensure := func(archive, line string) *fileLine {
		id := fileLineID(archive, line)
		if flines[id] == nil {
			flines[id] = &fileLine{id: id, archive: archive, number: line}
		}
		return flines[id]
	}
	for _, r := range in.Records {
		records[r.Key.ID()] = r
		f := ensure(r.Key.Archive, Line(r.Key.Number))
		f.events = append(f.events, r)
	}
	// Branch symbols can identify empty lines whose seeds must still exist.
	memberships := map[string]map[string]bool{}
	for _, s := range in.Symbols {
		if !s.Branch {
			continue
		}
		f := ensure(s.Archive, s.Target)
		if memberships[s.Name] == nil {
			memberships[s.Name] = map[string]bool{}
		}
		memberships[s.Name][f.id] = true
	}
	for _, f := range flines {
		if f.number != "trunk" {
			r, ok := records[(Key{p.Source, f.archive, Fork(f.number)}).ID()]
			if !ok {
				return nil, fmt.Errorf("missing forkpoint")
			}
			f.fork = &r
		}
		// Explicit links must describe one complete chain, including gaps. A
		// second root, fork inside a line, or disconnected tail is rejected.
		children := map[string]int{}
		roots := 0
		for _, r := range f.events {
			if r.Parent == nil {
				roots++
			} else if Line(r.Parent.Number) == f.number {
				children[r.Parent.ID()]++
				if children[r.Parent.ID()] > 1 {
					return nil, fmt.Errorf("fork inside file line")
				}
			} else {
				if f.fork == nil || *r.Parent != f.fork.Key {
					return nil, fmt.Errorf("wrong fork")
				}
				roots++
			}
		}
		if len(f.events) > 0 && roots != 1 {
			return nil, fmt.Errorf("incomplete file line ancestry")
		}
	}
	projects := map[string]*projectLine{}
	assignment := map[string]string{}
	trunk := &projectLine{id: "trunk"}
	if len(in.Records) > 0 {
		projects[trunk.id] = trunk
	}
	// Identical complete memberships form aliases. Any partial overlap fails.
	for _, name := range SortedKeys(memberships) {
		set := memberships[name]
		primary := ""
		for id := range set {
			if old := assignment[id]; old != "" {
				if primary != "" && primary != old {
					return nil, fmt.Errorf("overlapping branch symbols")
				}
				primary = old
			}
		}
		if primary != "" {
			line := projects[primary]
			if len(line.members) != len(set) {
				return nil, fmt.Errorf("partial branch alias")
			}
			for _, f := range line.members {
				if !set[f.id] {
					return nil, fmt.Errorf("partial branch alias")
				}
			}
			line.aliases = append(line.aliases, name)
			continue
		}
		line := &projectLine{id: "named\x00" + name, aliases: []string{name}}
		projects[line.id] = line
		for _, id := range SortedKeys(set) {
			line.members = append(line.members, flines[id])
			assignment[id] = line.id
		}
	}
	for _, id := range SortedKeys(flines) {
		f := flines[id]
		if assignment[id] != "" {
			continue
		}
		if f.number == "trunk" {
			trunk.members = append(trunk.members, f)
			assignment[id] = trunk.id
		} else {
			line := &projectLine{id: "numeric\x00" + id, members: []*fileLine{f}}
			projects[line.id] = line
			assignment[id] = line.id
		}
	}
	blobs := map[string]string{}
	// Source contents are not modified after normalization, so blobs share them
	// rather than holding a second copy of every revision body.
	for _, r := range in.Records {
		if r.Present {
			blobs[r.Key.ID()] = objects.putShared("blob", r.Content)
		}
	}
	return &structure{in: in, records: records, flines: flines, projects: projects, assignment: assignment, blobs: blobs}, nil
}

// Build always replays the complete normalized state. Nothing in its signature
// can expose a previously published tip or the number of intermediate syncs.
// Growing history starts from nothing here; Grow continues a published one.
func Build(input Inventory, p Policy) (Generation, error) {
	if p.Growing {
		return Grow(input, p, Published{})
	}
	g := Generation{Objects: Objects{}}
	s, err := analyze(input, p, g.Objects)
	if err != nil {
		return Generation{}, err
	}
	in, records, projects, assignment, blobs := s.in, s.records, s.projects, s.assignment, s.blobs
	g.Manifest = Manifest{Source: p.Source, PolicyDigest: p.Digest(), SourceDigest: in.Digest(p.Source), Verify: p.Verify, Revisions: map[string]RevisionEntry{}, Lines: map[string]string{}, Refs: map[string]string{}}
	events := map[string]string{}
	remaining := SortedKeys(projects)
	// lineParents records the primary parent line of every built line, which
	// ranks the candidates of the lines that branch from it.
	lineParents := map[string]string{}
	for len(remaining) > 0 {
		progress := false
		for i, id := range remaining {
			line := projects[id]
			// The files of one branch can fork from different lines: CVS
			// branches each file from whatever its working copy holds, such as
			// the vendor revision of a file never changed on trunk, or the
			// trunk revision of a file never changed on the parent branch.
			// Every such line must be built before this line's seed.
			candidates := map[string]int{}
			ready := true
			for _, f := range line.members {
				if f.fork == nil {
					continue
				}
				parent := assignment[fileLineID(f.archive, Line(f.fork.Key.Number))]
				if parent == "" || parent == id {
					return Generation{}, fmt.Errorf("unresolved parent line")
				}
				candidates[parent]++
				if g.Manifest.Lines[parent] == "" {
					ready = false
				}
			}
			if !ready {
				continue
			}
			parentLine := primaryParentLine(candidates, projects, lineParents)
			lineParents[id] = parentLine
			files := newLineTree(g.Objects)
			var parent string
			if id == "trunk" {
				message, when := "cvsgitrelay-root-v2\nline="+Hex(id)+"\n", SyntheticTime
				if p.Version() == 3 {
					message, when = p.lineMessage(line), lineTime(line)
				}
				parent = g.Objects.SyntheticAs(p.Relay(), files.oid(), nil, message, when)
			} else {
				var anchor *Record
				var pairs []string
				for _, f := range line.members {
					if f.fork == nil {
						return Generation{}, fmt.Errorf("unsupported parentless seed")
					}
					r := f.fork
					// The seed descends from the primary parent line only. Forks
					// on other lines still contribute their contents to the tree.
					inPrimary := assignment[fileLineID(f.archive, Line(r.Key.Number))] == parentLine
					if inPrimary && (anchor == nil || eventCompare(*r, *anchor) < 0) {
						anchor = r
					}
					if r.State == "live" {
						if e := files.set(r.Path, blobs[r.Key.ID()], r.Executable); e != nil {
							return Generation{}, e
						}
					}
					pairs = append(pairs, string(append(frame(f.id), B(r.Key.Bytes())...)))
				}
				if anchor == nil {
					return Generation{}, fmt.Errorf("empty seed membership")
				}
				slices.Sort(pairs)
				message := "cvsgitrelay-seed-v2\nline=" + Hex(id) + "\nforkpoint-anchor=" + Hex(anchor.Key.ID()) + "\n"
				for _, pair := range pairs {
					message += "member=" + Hex(pair) + "\n"
				}
				when := SyntheticTime
				if p.Version() == 3 {
					message, when = p.lineMessage(line), lineTime(line)
				}
				anchorOID := events[anchor.Key.ID()]
				if anchorOID == "" {
					return Generation{}, fmt.Errorf("unresolved seed anchor")
				}
				parent = g.Objects.SyntheticAs(p.Relay(), files.oid(), []string{anchorOID}, message, when)
			}
			var pending []Record
			for _, f := range line.members {
				pending = append(pending, f.events...)
			}
			slices.SortFunc(pending, eventCompare)
			// Kahn's algorithm always emits the earliest ready event in eventCompare
			// order, which preserves the reference tie precedence. Ready events are
			// heap positions in the sorted slice; waiting events are released when
			// their parent is emitted.
			var queue readyQueue
			waiting := map[string][]int{}
			for i, r := range pending {
				if r.Parent == nil || events[r.Parent.ID()] != "" {
					queue.push(i)
				} else {
					waiting[r.Parent.ID()] = append(waiting[r.Parent.ID()], i)
				}
			}
			for range pending {
				if queue.Len() == 0 {
					return Generation{}, fmt.Errorf("cyclic ancestry")
				}
				r := pending[queue.pop()]
				if r.State == "dead" {
					files.remove(r.Path)
				} else if e := files.set(r.Path, blobs[r.Key.ID()], r.Executable); e != nil {
					return Generation{}, e
				}
				parent = g.Objects.Event(p, r, files.oid(), parent)
				events[r.Key.ID()] = parent
				g.Manifest.Revisions[r.Key.ID()] = RevisionEntry{r.Fingerprint(), parent}
				for _, child := range waiting[r.Key.ID()] {
					queue.push(child)
				}
				delete(waiting, r.Key.ID())
			}
			g.Manifest.Lines[id] = parent
			ref, e := EncodedRef("refs/cvsgitrelay/lines/", id)
			if e != nil {
				return Generation{}, e
			}
			g.Manifest.Refs[ref] = parent
			// Each selected branch name of the line becomes a branch ref; the
			// trunk is a line without aliases, selected as "trunk".
			names := line.aliases
			if id == "trunk" {
				names = []string{TrunkBranch}
			}
			for _, name := range names {
				// A real branch named like the trunk would make the map ambiguous.
				if id != "trunk" && name == TrunkBranch && p.BranchMapped() {
					return Generation{}, fmt.Errorf("a CVS branch named %q cannot be told apart from the trunk in a branch map", TrunkBranch)
				}
				ref, selected, e := p.branchRef(name, id == "trunk")
				if e != nil {
					return Generation{}, e
				}
				if selected {
					g.Manifest.Refs[ref] = parent
				}
			}
			remaining = slices.Delete(remaining, i, i+1)
			progress = true
			break
		}
		if !progress {
			return Generation{}, fmt.Errorf("cyclic project line dependencies")
		}
	}
	tags, err := tagSelections(in, records, p)
	if err != nil {
		return Generation{}, err
	}
	var tips []string
	for _, tip := range g.Manifest.Lines {
		tips = append(tips, tip)
	}
	for _, name := range SortedKeys(tags) {
		selected := tags[name]
		tree, selection, e := g.Objects.snapshotTree(selected, blobs)
		if e != nil {
			return Generation{}, e
		}
		var oid string
		if p.Version() == 3 {
			oid, e = g.tagCommitV3(p, p.tagMessage(name), selected, tree, events, nil)
			if e != nil {
				return Generation{}, e
			}
		} else {
			message := "cvsgitrelay-tag-v2\nname=" + Hex(name) + "\nselection-sha256=" + selection + "\n"
			oid = g.Objects.SyntheticAs(p.Relay(), tree, nil, message, SyntheticTime)
		}
		ref, e := p.TagRef(name)
		if e != nil {
			return Generation{}, e
		}
		g.Manifest.Refs[ref] = oid
		tips = append(tips, oid)
	}
	if len(events) != len(in.Records) {
		return Generation{}, fmt.Errorf("revision coverage mismatch")
	}
	anchor := g.Objects.Anchor(tips)
	var parents []string
	if anchor != "" {
		parents = []string{anchor}
	}
	manifest := g.Objects.Put("blob", g.Manifest.Bytes())
	tree, e := g.Objects.Tree(map[string]string{"manifest.v2": manifest})
	if e != nil {
		return Generation{}, e
	}
	g.State = g.Objects.Synthetic(tree, parents, "cvsgitrelay-state-v2\n")
	// Dead source bodies and intermediate encoder scratch objects need not
	// appear in Git's reachable graph. The comparison/verification boundary is
	// precisely the objects protected by the authoritative generation. One walk
	// serves both the reachability check and the pruning.
	reachable, err := g.reachableObjects()
	if err != nil {
		return Generation{}, err
	}
	if err := g.validateReachable(reachable); err != nil {
		return Generation{}, err
	}
	g.Objects = reachable
	return g, nil
}

// primaryParentLine picks the line a branch seed descends from when its files
// fork from several lines. The deepest line by ordinary branches wins, so a
// branch of a branch descends from that branch even where some files still
// fork from the trunk. Vendor lines add no depth and lose ties, because CVS's
// HEAD follows a vendor branch where trunk never changed: a branch taken from
// the trunk descends from the trunk even if most files fork from the vendor
// line. Member counts and then line IDs break remaining ties deterministically.
func primaryParentLine(candidates map[string]int, projects map[string]*projectLine, lineParents map[string]string) string {
	depth := func(id string) int {
		d := 0
		for ; id != "trunk" && id != ""; id = lineParents[id] {
			if !vendorLine(projects[id]) {
				d++
			}
		}
		return d
	}
	best := ""
	for _, id := range SortedKeys(candidates) {
		if best == "" {
			best = id
			continue
		}
		if c := cmp.Or(
			cmp.Compare(depth(id), depth(best)),
			-cmp.Compare(boolRank(vendorLine(projects[id])), boolRank(vendorLine(projects[best]))),
			cmp.Compare(candidates[id], candidates[best]),
		); c > 0 {
			best = id
		}
	}
	return best
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// vendorLine reports whether every file line of a branch has an odd branch
// number, as CVS gives vendor branches created by import (1.1.1 by default),
// while ordinary branches made by tag -b always get even numbers.
func vendorLine(line *projectLine) bool {
	if line.id == "trunk" || len(line.members) == 0 {
		return false
	}
	for _, f := range line.members {
		last := f.number[strings.LastIndexByte(f.number, '.')+1:]
		if (last[len(last)-1]-'0')%2 == 0 {
			return false
		}
	}
	return true
}

// tagSelections returns the revisions every tag selects, sorted by path.
// Snapshot tags select exact revision contents even across different lines.
func tagSelections(in Inventory, records map[string]Record, p Policy) (map[string][]Record, error) {
	tags := map[string][]Record{}
	for _, s := range in.Symbols {
		if !s.Branch {
			tags[s.Name] = append(tags[s.Name], records[(Key{p.Source, s.Archive, s.Target}).ID()])
		}
	}
	for _, selected := range tags {
		slices.SortFunc(selected, func(a, b Record) int { return strings.Compare(a.Path, b.Path) })
		for i, r := range selected {
			if i > 0 && selected[i-1].Path == r.Path {
				return nil, fmt.Errorf("duplicate tag path")
			}
		}
	}
	return tags, nil
}

// snapshotTree stores the tree of the revisions a tag selects and returns it
// with the digest of the selection, which names the revisions but not their
// contents.
func (o Objects) snapshotTree(selected []Record, blobs map[string]string) (tree, selection string, err error) {
	b := U(uint64(len(selected)))
	files := map[string]string{}
	executable := map[string]bool{}
	for _, r := range selected {
		pair := append(frame(r.Path), B(r.Key.Bytes())...)
		b = append(b, B(pair)...)
		if r.State == "live" {
			files[r.Path] = blobs[r.Key.ID()]
			executable[r.Path] = r.Executable
		}
	}
	tree, err = o.TreeModes(files, executable)
	return tree, Hash("cvsgitrelay-tag-selection-v2", b), err
}

// tagCommitV3 attaches a format 3 tag snapshot to the history it was taken
// from. The base is the event commit of the newest selected revision, the
// one that also dates the tag. If the tree there already equals the snapshot,
// the tag names that event commit itself. Otherwise a synthetic commit on top
// of it records the exact selection, so the tag still has the event history
// as its ancestry instead of being a parentless root. A base that this
// generation did not create is looked up in published.
func (g Generation) tagCommitV3(p Policy, message string, selected []Record, tree string, events map[string]string, published Objects) (string, error) {
	newest := slices.MaxFunc(selected, eventCompare)
	base := events[newest.Key.ID()]
	if base == "" {
		return "", fmt.Errorf("unresolved tag base")
	}
	commit, ok := g.Objects[base]
	if !ok {
		commit, ok = published[base]
	}
	if !ok || commit.Kind != "commit" {
		return "", fmt.Errorf("unavailable tag base")
	}
	if commitTree(commit.Data) == tree {
		return base, nil
	}
	return g.Objects.SyntheticAs(p.Relay(), tree, []string{base}, message, tagTime(selected)), nil
}

// commitTree returns the tree OID in the header of the commit bytes.
func commitTree(commit []byte) string {
	tree, _, _ := bytes.Cut(bytes.TrimPrefix(commit, []byte("tree ")), []byte("\n"))
	return string(tree)
}

func (g Generation) reachableObjects() (Objects, error) {
	seen := Objects{}
	pending := []string{g.State}
	for len(pending) > 0 {
		oid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, ok := seen[oid]; ok {
			continue
		}
		o, ok := g.Objects[oid]
		if !ok || OID(o.Kind, o.Data) != oid {
			return nil, fmt.Errorf("invalid reachable object")
		}
		seen[oid] = o
		switch o.Kind {
		case "blob":
			continue
		case "tree":
			data := o.Data
			for len(data) > 0 {
				end := bytes.IndexByte(data, 0)
				if end < 0 || len(data) < end+21 {
					return nil, fmt.Errorf("invalid tree encoding")
				}
				pending = append(pending, hex.EncodeToString(data[end+1:end+21]))
				data = data[end+21:]
			}
			continue
		case "commit":
		default:
			return nil, fmt.Errorf("unsupported canonical object type")
		}
		head, _, ok := bytes.Cut(o.Data, []byte("\n\n"))
		if !ok {
			return nil, fmt.Errorf("invalid commit")
		}
		for _, line := range strings.Split(string(head), "\n") {
			if tree, ok := strings.CutPrefix(line, "tree "); ok {
				pending = append(pending, tree)
			}
			if p, ok := strings.CutPrefix(line, "parent "); ok {
				pending = append(pending, p)
			}
		}
	}
	return seen, nil
}

// ValidateReachability follows real commit/tree edges; manifest text is not a
// GC root. Revision events and every logical projection must remain commits.
func (g Generation) ValidateReachability() error {
	seen, err := g.reachableObjects()
	if err != nil {
		return err
	}
	return g.validateReachable(seen)
}

// validateReachable checks revision events and projections against the result
// of an existing reachability walk.
func (g Generation) validateReachable(seen Objects) error {
	for _, r := range g.Manifest.Revisions {
		if seen[r.Event].Kind != "commit" {
			return fmt.Errorf("unreachable revision event")
		}
	}
	for _, tip := range g.Manifest.Refs {
		if seen[tip].Kind != "commit" {
			return fmt.Errorf("unreachable projection")
		}
	}
	return nil
}
