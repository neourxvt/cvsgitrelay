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

// Build always replays the complete normalized state. Nothing in its signature
// can expose a previously published tip or the number of intermediate syncs.
func Build(input Inventory, p Policy) (Generation, error) {
	in, err := input.Normalize(p)
	if err != nil {
		return Generation{}, err
	}
	if _, named := p.Authors[RelayAuthor]; named && p.Version() == 3 {
		for _, r := range in.Records {
			if r.Author == RelayAuthor {
				return Generation{}, fmt.Errorf("the CVS username %q is reserved for the relay identity in the author map", RelayAuthor)
			}
		}
	}
	g := Generation{Objects: Objects{}, Manifest: Manifest{Source: p.Source, PolicyDigest: p.Digest(), SourceDigest: in.Digest(p.Source), Verify: p.Verify, Revisions: map[string]RevisionEntry{}, Lines: map[string]string{}, Refs: map[string]string{}}}
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
				return Generation{}, fmt.Errorf("missing forkpoint")
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
					return Generation{}, fmt.Errorf("fork inside file line")
				}
			} else {
				if f.fork == nil || *r.Parent != f.fork.Key {
					return Generation{}, fmt.Errorf("wrong fork")
				}
				roots++
			}
		}
		if len(f.events) > 0 && roots != 1 {
			return Generation{}, fmt.Errorf("incomplete file line ancestry")
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
					return Generation{}, fmt.Errorf("overlapping branch symbols")
				}
				primary = old
			}
		}
		if primary != "" {
			line := projects[primary]
			if len(line.members) != len(set) {
				return Generation{}, fmt.Errorf("partial branch alias")
			}
			for _, f := range line.members {
				if !set[f.id] {
					return Generation{}, fmt.Errorf("partial branch alias")
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
	events := map[string]string{}
	blobs := map[string]string{}
	// Source contents are not modified after normalization, so blobs share them
	// rather than holding a second copy of every revision body.
	for _, r := range in.Records {
		if r.Present {
			blobs[r.Key.ID()] = g.Objects.putShared("blob", r.Content)
		}
	}
	remaining := SortedKeys(projects)
	for len(remaining) > 0 {
		progress := false
		for i, id := range remaining {
			line := projects[id]
			parentLine := ""
			ready := true
			for _, f := range line.members {
				if f.fork == nil {
					continue
				}
				parent := assignment[fileLineID(f.archive, Line(f.fork.Key.Number))]
				if parent == "" || parent == id {
					return Generation{}, fmt.Errorf("unresolved parent line")
				}
				if parentLine != "" && parentLine != parent {
					return Generation{}, fmt.Errorf("conflicting parent lines")
				}
				parentLine = parent
				if g.Manifest.Lines[parent] == "" {
					ready = false
				}
			}
			if !ready {
				continue
			}
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
					if anchor == nil || eventCompare(*r, *anchor) < 0 {
						anchor = r
					}
					if r.State == "live" {
						if e := files.set(r.Path, blobs[r.Key.ID()]); e != nil {
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
				} else if e := files.set(r.Path, blobs[r.Key.ID()]); e != nil {
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
				ref, selected, e := p.BranchRef(name)
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
	// Snapshot tags select exact revision contents even across different lines.
	tags := map[string][]Record{}
	for _, s := range in.Symbols {
		if !s.Branch {
			tags[s.Name] = append(tags[s.Name], records[(Key{p.Source, s.Archive, s.Target}).ID()])
		}
	}
	var tips []string
	for _, tip := range g.Manifest.Lines {
		tips = append(tips, tip)
	}
	for _, name := range SortedKeys(tags) {
		selected := tags[name]
		slices.SortFunc(selected, func(a, b Record) int { return strings.Compare(a.Path, b.Path) })
		b := U(uint64(len(selected)))
		files := map[string]string{}
		for i, r := range selected {
			if i > 0 && selected[i-1].Path == r.Path {
				return Generation{}, fmt.Errorf("duplicate tag path")
			}
			pair := append(frame(r.Path), B(r.Key.Bytes())...)
			b = append(b, B(pair)...)
			if r.State == "live" {
				files[r.Path] = blobs[r.Key.ID()]
			}
		}
		tree, e := g.Objects.Tree(files)
		if e != nil {
			return Generation{}, e
		}
		message := "cvsgitrelay-tag-v2\nname=" + Hex(name) + "\nselection-sha256=" + Hash("cvsgitrelay-tag-selection-v2", b) + "\n"
		when := SyntheticTime
		if p.Version() == 3 {
			message, when = p.tagMessage(name), tagTime(selected)
		}
		oid := g.Objects.SyntheticAs(p.Relay(), tree, nil, message, when)
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
