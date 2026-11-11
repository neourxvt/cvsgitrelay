package canonical

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Growing history never changes what it published. Each sync continues the
// published generation: new CVS revisions become changeset commits on top of
// their line, and wherever the tip of a line no longer has the files CVS has
// now, because published history changed in CVS, a correction commit brings
// it there. The tip of every line therefore always matches the current
// source, while every published commit, branch and tag stays as it is.
//
// The manifest records what was published: every revision with its commit,
// the line each file line belongs to, the line each branch ref follows and
// the versions of every tag. A file line never moves to another line, a
// branch ref only moves forward, and a tag ref never moves. A branch or tag
// that CVS deleted keeps its last value. A branch that CVS created again
// starts a new line whose first commit also descends from the old ref value,
// and a tag that CVS moved is published as a new version under a new ref.

// Published is the generation a growing build continues: its state commit,
// its manifest and a reader for its objects.
type Published struct {
	State    string
	Manifest Manifest
	// Load reads published objects by ID. Grow reads the tip commits of the
	// lines, the trees it continues and the bases of new tags through it.
	Load func(oids []string) (Objects, error)
}

// correctionMessage is the message of a commit that brings a line to the
// current source after published history changed in CVS.
const correctionMessage = "Update files to their current CVS state\n\n" +
	"CVS changed history that was already published, for example through\n" +
	"cvs admin, a changed file mode, a removed archive or a branch tag added\n" +
	"to more files. This commit brings the files to the state CVS has now.\n"

type grower struct {
	p    Policy
	g    *Generation
	s    *structure
	prev Published
	// pm is the published manifest and m the one being built.
	pm, m Manifest
	// known holds the published objects read so far.
	known Objects
	// events maps the ID of every published or new revision to its commit.
	events map[string]string
	// lines are the lines of the current source by ID, and lineOf maps
	// every file line of the source to its line.
	lines  map[string]*projectLine
	lineOf map[string]string
	// sourceTime is the time of the newest revision of the source.
	sourceTime int64
}

// Grow continues the published generation prev with the current source. An
// empty prev starts a new history. When the source asks for nothing new, the
// result is prev itself.
func Grow(input Inventory, p Policy, prev Published) (Generation, error) {
	if !p.Growing {
		return Generation{}, fmt.Errorf("the policy does not keep a growing history")
	}
	g := Generation{Objects: Objects{}}
	s, err := analyze(input, p, g.Objects)
	if err != nil {
		return Generation{}, err
	}
	pm := prev.Manifest
	if prev.State == "" {
		pm = growingManifest(Manifest{Source: p.Source, PolicyDigest: p.Digest(), Verify: p.Verify, Revisions: map[string]RevisionEntry{}, Lines: map[string]string{}, Refs: map[string]string{}})
	} else if !pm.Growing || pm.Source != p.Source || pm.PolicyDigest != p.Digest() || pm.Verify != p.Verify {
		return Generation{}, fmt.Errorf("published generation has a different policy")
	}
	gr := &grower{p: p, g: &g, s: s, prev: prev, pm: pm, known: Objects{}, events: map[string]string{}}
	gr.m = Manifest{Source: p.Source, PolicyDigest: p.Digest(), SourceDigest: s.in.Digest(p.Source), Verify: p.Verify, Growing: true,
		Revisions: maps.Clone(pm.Revisions), Lines: maps.Clone(pm.Lines), Refs: maps.Clone(pm.Refs),
		Removed: maps.Clone(pm.Removed), Members: maps.Clone(pm.Members), Bindings: maps.Clone(pm.Bindings), Tags: maps.Clone(pm.Tags)}
	for id, entry := range pm.Revisions {
		gr.events[id] = entry.Event
	}
	// Published revisions keep their commits. A changed one only gets its
	// new fingerprint, so that the change is reported once; one the source
	// no longer lists is marked removed until it comes back.
	listed := map[string]bool{}
	for _, r := range s.in.Records {
		id := r.Key.ID()
		listed[id] = true
		gr.sourceTime = max(gr.sourceTime, r.Time)
		if entry, ok := gr.m.Revisions[id]; ok {
			entry.Fingerprint = r.Fingerprint()
			gr.m.Revisions[id] = entry
			delete(gr.m.Removed, id)
		}
	}
	for id := range gr.m.Revisions {
		if !listed[id] {
			gr.m.Removed[id] = true
		}
	}
	if err := gr.resolveLines(); err != nil {
		return Generation{}, err
	}
	// The tip commits tell which lines still have the files of the source.
	var tips []string
	for id := range gr.lines {
		if tip := pm.Lines[id]; tip != "" {
			tips = append(tips, tip)
		}
	}
	if err := gr.load(tips); err != nil {
		return Generation{}, err
	}
	if err := gr.growLines(); err != nil {
		return Generation{}, err
	}
	if err := gr.growTags(); err != nil {
		return Generation{}, err
	}
	for id, line := range gr.lineOf {
		gr.m.Members[id] = line
	}
	for _, r := range s.in.Records {
		if gr.m.Revisions[r.Key.ID()].Event == "" {
			return Generation{}, fmt.Errorf("revision coverage mismatch")
		}
	}
	if prev.State != "" && bytes.Equal(gr.m.Bytes(), pm.Bytes()) {
		return Generation{State: prev.State, Manifest: pm, Objects: Objects{}}, nil
	}
	return gr.finish()
}

// resolveLines assigns every file line of the source to a line. A published
// file line stays on its line. The project line that the current symbols
// form joins the line of its published file lines, so a branch that CVS
// extends to more files or a file line whose branch symbol CVS deleted stays
// where it was. A project line without published file lines is new; if its
// ID is taken by a published line, as when CVS deleted a branch and created
// it again, it gets a numbered one.
func (gr *grower) resolveLines() error {
	gr.lines, gr.lineOf = map[string]*projectLine{}, map[string]string{}
	taken := func(id string) bool { return gr.pm.Lines[id] != "" || gr.lines[id] != nil }
	for _, pid := range SortedKeys(gr.s.projects) {
		project := gr.s.projects[pid]
		published := map[string]bool{}
		for _, f := range project.members {
			if id, ok := gr.pm.Members[f.id]; ok {
				published[id] = true
			}
		}
		id := pid
		switch {
		case pid == "trunk":
			if len(published) > 1 || (len(published) == 1 && !published["trunk"]) {
				return fmt.Errorf("published trunk file lines left the trunk")
			}
		case len(published) > 1:
			return fmt.Errorf("CVS branch symbols join file lines of different published branches")
		case len(published) == 1:
			id = SortedKeys(published)[0]
		default:
			for n := 2; taken(id); n++ {
				id = pid + "\x00" + strconv.Itoa(n)
			}
		}
		line := gr.lines[id]
		if line == nil {
			line = &projectLine{id: id, label: pid}
			gr.lines[id] = line
		}
		line.aliases = append(line.aliases, project.aliases...)
		for _, f := range project.members {
			line.members = append(line.members, f)
			gr.lineOf[f.id] = id
		}
	}
	for _, line := range gr.lines {
		slices.SortFunc(line.members, func(a, b *fileLine) int { return strings.Compare(a.id, b.id) })
		slices.Sort(line.aliases)
	}
	return nil
}

// growLines grows every line after the lines its files fork from, so that
// the first commit of a new line can descend from its fork point.
func (gr *grower) growLines() error {
	remaining := SortedKeys(gr.lines)
	lineParents := map[string]string{}
	done := map[string]bool{}
	for len(remaining) > 0 {
		progress := false
		for i, id := range remaining {
			line := gr.lines[id]
			candidates := map[string]int{}
			ready := true
			for _, f := range line.members {
				if f.fork == nil {
					continue
				}
				parent := gr.lineOf[fileLineID(f.archive, Line(f.fork.Key.Number))]
				if parent == "" {
					return fmt.Errorf("unresolved parent line")
				}
				// A published line can hold a file line and its own branch
				// once CVS regrouped them; only a new line needs another
				// line to start from.
				if parent == id {
					if gr.pm.Lines[id] == "" {
						return fmt.Errorf("unresolved parent line")
					}
					continue
				}
				candidates[parent]++
				if !done[parent] {
					ready = false
				}
			}
			if !ready {
				continue
			}
			lineParents[id] = primaryParentLine(candidates, gr.lines, lineParents)
			if err := gr.growLine(line, lineParents[id]); err != nil {
				return err
			}
			done[id] = true
			remaining = slices.Delete(remaining, i, i+1)
			progress = true
			break
		}
		if !progress {
			return fmt.Errorf("cyclic project line dependencies")
		}
	}
	return nil
}

// growLine adds the new revisions of one line as changesets, then a
// correction commit if the files still differ from the source, and points the
// line's refs at the result.
func (gr *grower) growLine(line *projectLine, parentLine string) error {
	p, id := gr.p, line.id
	// A branch ref that followed another line until now must keep its old
	// value in the ancestry of this line.
	names := line.aliases
	if id == "trunk" {
		names = []string{TrunkBranch}
	}
	var refs, rebound []string
	for _, name := range names {
		if id != "trunk" && name == TrunkBranch && p.BranchMapped() {
			return fmt.Errorf("a CVS branch named %q cannot be told apart from the trunk in a branch map", TrunkBranch)
		}
		ref, selected, err := p.branchRef(name, id == "trunk")
		if err != nil {
			return err
		}
		if !selected {
			continue
		}
		refs = append(refs, ref)
		if old, ok := gr.pm.Bindings[ref]; ok && old != id {
			rebound = append(rebound, gr.pm.Refs[ref])
		}
	}
	slices.Sort(rebound)
	rebound = slices.Compact(rebound)
	var pending []Record
	for _, f := range line.members {
		for _, r := range f.events {
			if _, published := gr.pm.Revisions[r.Key.ID()]; !published {
				pending = append(pending, r)
			}
		}
	}
	target, err := gr.targetTree(line)
	if err != nil {
		return err
	}
	tip := gr.pm.Lines[id]
	if tip != "" && len(pending) == 0 && len(rebound) == 0 && commitTree(gr.object(tip).Data) == target {
		return gr.publishLine(id, tip, refs)
	}
	var files *lineTree
	var parent string
	if tip != "" {
		if files, err = gr.loadLineTree(tip); err != nil {
			return err
		}
		parent = tip
	} else {
		if files, parent, err = gr.seed(line, parentLine, rebound); err != nil {
			return err
		}
		rebound = nil
	}
	groups, err := groupChangesets(pending, gr.events)
	if err != nil {
		return err
	}
	for _, c := range groups {
		if err := c.apply(files, gr.s.blobs); err != nil {
			return err
		}
		first := c.members[0]
		parent = gr.g.Objects.Put("commit", CommitBytes(files.oid(), []string{parent}, p.EventAuthor(first.Author), p.Relay(), c.time(), p.changesetMessage(c.members)))
		for _, r := range c.members {
			gr.events[r.Key.ID()] = parent
			gr.m.Revisions[r.Key.ID()] = RevisionEntry{r.Fingerprint(), parent}
		}
	}
	// Corrections and continuations are dated no earlier than the source's
	// newest revision, after which CVS made the change they record.
	when := max(gr.sourceTime, committerTime(gr.object(parent).Data))
	if files.oid() != target {
		parent = gr.g.Objects.SyntheticAs(p.Relay(), target, []string{parent}, correctionMessage, when)
		gr.g.Corrections++
	}
	if len(rebound) > 0 {
		message := "CVS branch " + textField(names[0]) + " continues here\n"
		parent = gr.g.Objects.SyntheticAs(p.Relay(), target, append([]string{parent}, rebound...), message, when)
	}
	return gr.publishLine(id, parent, refs)
}

// publishLine points the line and its selected branch refs at tip.
func (gr *grower) publishLine(id, tip string, refs []string) error {
	gr.m.Lines[id] = tip
	ref, err := EncodedRef("refs/cvsgitrelay/lines/", id)
	if err != nil {
		return err
	}
	gr.m.Refs[ref] = tip
	for _, ref := range refs {
		gr.m.Refs[ref] = tip
		gr.m.Bindings[ref] = id
	}
	return nil
}

// seed starts a new line as Build does: the trunk with an empty root commit,
// and a branch with a commit that holds the fork points of its files and
// descends from the earliest of them on its primary parent line. A branch
// that CVS created again also descends from the old values of its refs.
func (gr *grower) seed(line *projectLine, parentLine string, rebound []string) (*lineTree, string, error) {
	p := gr.p
	files := newLineTree(gr.g.Objects)
	if line.id == "trunk" {
		return files, gr.g.Objects.SyntheticAs(p.Relay(), files.oid(), nil, p.lineMessage(line), lineTime(line)), nil
	}
	var anchor *Record
	for _, f := range line.members {
		if f.fork == nil {
			return nil, "", fmt.Errorf("unsupported parentless seed")
		}
		r := f.fork
		inPrimary := gr.lineOf[fileLineID(f.archive, Line(r.Key.Number))] == parentLine
		if inPrimary && (anchor == nil || eventCompare(*r, *anchor) < 0) {
			anchor = r
		}
		if r.State == "live" {
			if err := files.set(r.Path, gr.s.blobs[r.Key.ID()], r.Executable); err != nil {
				return nil, "", err
			}
		}
	}
	if anchor == nil {
		return nil, "", fmt.Errorf("empty seed membership")
	}
	anchorOID := gr.events[anchor.Key.ID()]
	if anchorOID == "" {
		return nil, "", fmt.Errorf("unresolved seed anchor")
	}
	parents := append([]string{anchorOID}, rebound...)
	return files, gr.g.Objects.SyntheticAs(p.Relay(), files.oid(), parents, p.lineMessage(line), lineTime(line)), nil
}

// targetTree stores the tree a line has in the current source: for every file
// line, its newest revision, or its fork point if it has none yet.
func (gr *grower) targetTree(line *projectLine) (string, error) {
	files := newLineTree(gr.g.Objects)
	for _, f := range line.members {
		// Revision numbers grow along the chain of a file line.
		r := f.fork
		if len(f.events) > 0 {
			newest := slices.MaxFunc(f.events, func(a, b Record) int { return CompareNumber(a.Key.Number, b.Key.Number) })
			r = &newest
		}
		if r != nil && r.State == "live" {
			if err := files.set(r.Path, gr.s.blobs[r.Key.ID()], r.Executable); err != nil {
				return "", err
			}
		}
	}
	return files.oid(), nil
}

// growTags publishes the tags that are new or that CVS moved. A published
// version never changes, even if the contents of its revisions do.
func (gr *grower) growTags() error {
	p := gr.p
	tags, err := tagSelections(gr.s.in, gr.s.records, p)
	if err != nil {
		return err
	}
	type version struct {
		name, tree, selection string
		selected              []Record
	}
	var versions []version
	var bases []string
	for _, name := range SortedKeys(tags) {
		selected := tags[name]
		tree, selection, err := gr.g.Objects.snapshotTree(selected, gr.s.blobs)
		if err != nil {
			return err
		}
		if gr.m.Tags[name].Selection == selection {
			continue
		}
		versions = append(versions, version{name, tree, selection, selected})
		bases = append(bases, gr.events[slices.MaxFunc(selected, eventCompare).Key.ID()])
	}
	if err := gr.load(bases); err != nil {
		return err
	}
	for _, v := range versions {
		n := gr.m.Tags[v.name].Versions + 1
		ref, err := p.TagVersionRef(v.name, n)
		if err != nil {
			return err
		}
		if gr.m.Refs[ref] != "" {
			return fmt.Errorf("tag version ref already exists")
		}
		message := p.tagMessage(v.name)
		if n > 1 {
			message = "CVS tag " + textField(v.name) + ", version " + strconv.Itoa(n) + "\n"
		}
		oid, err := gr.g.tagCommitV3(p, message, v.selected, v.tree, gr.events, gr.known)
		if err != nil {
			return err
		}
		gr.m.Refs[ref] = oid
		gr.m.Tags[v.name] = TagEntry{n, v.selection}
	}
	return nil
}

// finish writes the state commit, which descends from every published value
// and from the previous state, so the state ref only moves forward as well,
// and keeps only the new objects it reaches.
func (gr *grower) finish() (Generation, error) {
	g := gr.g
	g.Manifest = gr.m
	var tips []string
	for _, tip := range gr.m.Refs {
		tips = append(tips, tip)
	}
	var parents []string
	if anchor := g.Objects.Anchor(tips); anchor != "" {
		parents = append(parents, anchor)
	}
	if gr.prev.State != "" {
		parents = append(parents, gr.prev.State)
	}
	manifest := g.Objects.Put("blob", gr.m.Bytes())
	tree, err := g.Objects.Tree(map[string]string{"manifest.v2": manifest})
	if err != nil {
		return Generation{}, err
	}
	g.State = g.Objects.Synthetic(tree, parents, "cvsgitrelay-state-v2\n")
	// Objects outside g.Objects were published before; the walk stops there.
	seen := Objects{}
	pending := []string{g.State}
	for len(pending) > 0 {
		oid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		o, ok := g.Objects[oid]
		if _, done := seen[oid]; done || !ok {
			continue
		}
		seen[oid] = o
		children, err := objectLinks(o)
		if err != nil {
			return Generation{}, err
		}
		pending = append(pending, children...)
	}
	published := map[string]bool{gr.prev.State: true}
	for _, entry := range gr.pm.Revisions {
		published[entry.Event] = true
	}
	for _, tip := range gr.pm.Refs {
		published[tip] = true
	}
	for _, entry := range g.Manifest.Revisions {
		if seen[entry.Event].Kind != "commit" && !published[entry.Event] {
			return Generation{}, fmt.Errorf("unreachable revision event")
		}
	}
	for _, tip := range g.Manifest.Refs {
		if seen[tip].Kind != "commit" && !published[tip] {
			return Generation{}, fmt.Errorf("unreachable projection")
		}
	}
	g.Objects = seen
	return *g, nil
}

// objectLinks returns the objects a tree or commit refers to.
func objectLinks(o Object) ([]string, error) {
	var links []string
	switch o.Kind {
	case "blob":
	case "tree":
		entries, err := treeEntries(o.Data)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			links = append(links, e.oid)
		}
	case "commit":
		head, _, ok := bytes.Cut(o.Data, []byte("\n\n"))
		if !ok {
			return nil, fmt.Errorf("invalid commit")
		}
		for _, line := range strings.Split(string(head), "\n") {
			if oid, ok := strings.CutPrefix(line, "tree "); ok {
				links = append(links, oid)
			}
			if oid, ok := strings.CutPrefix(line, "parent "); ok {
				links = append(links, oid)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported canonical object type")
	}
	return links, nil
}

// gitTreeEntry is one entry of a Git tree object.
type gitTreeEntry struct {
	mode, name, oid string
	raw             [20]byte
}

// treeEntries parses a Git tree object.
func treeEntries(data []byte) ([]gitTreeEntry, error) {
	var entries []gitTreeEntry
	for len(data) > 0 {
		end := bytes.IndexByte(data, 0)
		if end < 0 || len(data) < end+21 {
			return nil, fmt.Errorf("invalid tree encoding")
		}
		mode, name, ok := strings.Cut(string(data[:end]), " ")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid tree entry")
		}
		e := gitTreeEntry{mode: mode, name: name, oid: hex.EncodeToString(data[end+1 : end+21])}
		copy(e.raw[:], data[end+1:end+21])
		entries = append(entries, e)
		data = data[end+21:]
	}
	return entries, nil
}

// load reads the given published objects that are not known yet. Every
// object must hash to its ID.
func (gr *grower) load(oids []string) error {
	var need []string
	for _, oid := range oids {
		_, known := gr.known[oid]
		_, built := gr.g.Objects[oid]
		if oid != "" && !known && !built {
			need = append(need, oid)
		}
	}
	slices.Sort(need)
	need = slices.Compact(need)
	if len(need) == 0 {
		return nil
	}
	if gr.prev.Load == nil {
		return fmt.Errorf("published objects are unavailable")
	}
	got, err := gr.prev.Load(need)
	if err != nil {
		return err
	}
	for _, oid := range need {
		o, ok := got[oid]
		if !ok || OID(o.Kind, o.Data) != oid {
			return fmt.Errorf("published object is missing or corrupt")
		}
		gr.known[oid] = o
	}
	return nil
}

// object returns a new or loaded object; it must have been loaded before.
func (gr *grower) object(oid string) Object {
	if o, ok := gr.g.Objects[oid]; ok {
		return o
	}
	return gr.known[oid]
}

// loadLineTree reads the tree of a published commit into a lineTree. Its
// directories keep their IDs, so only those that change are written again.
func (gr *grower) loadLineTree(commit string) (*lineTree, error) {
	if err := gr.load([]string{commit}); err != nil {
		return nil, err
	}
	c := gr.object(commit)
	if c.Kind != "commit" {
		return nil, fmt.Errorf("published tip is not a commit")
	}
	root := commitTree(c.Data)
	// Read the trees level by level, so that each level is one batch.
	for level := []string{root}; len(level) > 0; {
		if err := gr.load(level); err != nil {
			return nil, err
		}
		var next []string
		for _, oid := range level {
			o := gr.object(oid)
			if o.Kind != "tree" {
				return nil, fmt.Errorf("published tree is not a tree")
			}
			entries, err := treeEntries(o.Data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.mode == "40000" {
					next = append(next, e.oid)
				}
			}
		}
		level = next
	}
	dir, err := gr.treeDir(root)
	if err != nil {
		return nil, err
	}
	return &lineTree{objects: gr.g.Objects, root: dir}, nil
}

// treeDir builds the lineTree directory of a loaded tree object.
func (gr *grower) treeDir(oid string) (*treeDir, error) {
	entries, err := treeEntries(gr.object(oid).Data)
	if err != nil {
		return nil, err
	}
	d := newTreeDir()
	d.oid = oid
	if _, err := hex.Decode(d.raw[:], []byte(oid)); err != nil {
		return nil, err
	}
	for _, e := range entries {
		switch e.mode {
		case "40000":
			sub, err := gr.treeDir(e.oid)
			if err != nil {
				return nil, err
			}
			d.entries[e.name+"/"] = &treeEntry{name: e.name, dir: sub}
			d.order = append(d.order, e.name+"/")
		case "100644", "100755":
			d.entries[e.name] = &treeEntry{name: e.name, blob: e.raw, executable: e.mode == "100755"}
			d.order = append(d.order, e.name)
		default:
			return nil, fmt.Errorf("unsupported published tree entry")
		}
	}
	slices.Sort(d.order)
	return d, nil
}

// committerTime returns the committer time of commit bytes.
func committerTime(commit []byte) int64 {
	head, _, _ := bytes.Cut(commit, []byte("\n\n"))
	for _, line := range strings.Split(string(head), "\n") {
		if rest, ok := strings.CutPrefix(line, "committer "); ok {
			fields := strings.Fields(rest)
			if len(fields) >= 2 {
				t, _ := strconv.ParseInt(fields[len(fields)-2], 10, 64)
				return t
			}
		}
	}
	return 0
}
