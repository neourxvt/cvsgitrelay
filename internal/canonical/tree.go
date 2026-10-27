package canonical

import (
	"container/heap"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// lineTree incrementally maintains the checkout tree of one line during replay.
// Only directories on a changed path are re-encoded; unchanged subtrees reuse
// their cached IDs. Every emitted object is byte-identical to the one that
// Objects.Tree produces for the same complete file map.
type lineTree struct {
	objects Objects
	root    *treeDir
	scratch []byte
}

type treeDir struct {
	// Keys are sort names: a file's name, or a directory's name plus "/".
	entries map[string]*treeEntry
	order   []string
	oid     string // cached hex ID; empty while the directory has changes
	raw     [20]byte
}

type treeEntry struct {
	name string
	blob [20]byte
	dir  *treeDir
}

func newTreeDir() *treeDir { return &treeDir{entries: map[string]*treeEntry{}} }

func newLineTree(o Objects) *lineTree { return &lineTree{objects: o, root: newTreeDir()} }

func (d *treeDir) insert(key string, e *treeEntry) {
	i, _ := slices.BinarySearch(d.order, key)
	d.order = slices.Insert(d.order, i, key)
	d.entries[key] = e
}

func (d *treeDir) remove(key string) {
	if i, found := slices.BinarySearch(d.order, key); found {
		d.order = slices.Delete(d.order, i, i+1)
	}
	delete(d.entries, key)
}

// set adds or replaces one file, applying the same path, ID and file/directory
// conflict rules as Objects.Tree.
func (t *lineTree) set(path, oid string) error {
	if err := Path(path); err != nil {
		return err
	}
	if !ValidOID(oid) {
		return fmt.Errorf("invalid tree blob")
	}
	var raw [20]byte
	_, _ = hex.Decode(raw[:], []byte(oid))
	parts := strings.Split(path, "/")
	d := t.root
	trail := []*treeDir{d}
	for _, part := range parts[:len(parts)-1] {
		if d.entries[part] != nil {
			return fmt.Errorf("file/directory conflict")
		}
		e := d.entries[part+"/"]
		if e == nil {
			e = &treeEntry{name: part, dir: newTreeDir()}
			d.insert(part+"/", e)
		}
		d = e.dir
		trail = append(trail, d)
	}
	base := parts[len(parts)-1]
	if d.entries[base+"/"] != nil {
		return fmt.Errorf("directory/file conflict")
	}
	if e := d.entries[base]; e != nil {
		if e.blob == raw {
			return nil
		}
		e.blob = raw
	} else {
		d.insert(base, &treeEntry{name: base, blob: raw})
	}
	for _, dir := range trail {
		dir.oid = ""
	}
	return nil
}

// remove deletes one file if present and prunes directories left empty, as a
// tree rebuilt from the reduced file map would never contain them.
func (t *lineTree) remove(path string) {
	parts := strings.Split(path, "/")
	d := t.root
	trail := []*treeDir{d}
	for _, part := range parts[:len(parts)-1] {
		e := d.entries[part+"/"]
		if e == nil {
			return
		}
		d = e.dir
		trail = append(trail, d)
	}
	base := parts[len(parts)-1]
	if d.entries[base] == nil {
		return
	}
	d.remove(base)
	for i := len(trail) - 1; i > 0 && len(trail[i].entries) == 0; i-- {
		trail[i-1].remove(parts[i-1] + "/")
	}
	for _, dir := range trail {
		dir.oid = ""
	}
}

// oid stores every changed tree object and returns the root tree ID.
func (t *lineTree) oid() string { return t.emit(t.root) }

func (t *lineTree) emit(d *treeDir) string {
	if d.oid != "" {
		return d.oid
	}
	// Children first: the shared scratch buffer then holds only this directory.
	for _, key := range d.order {
		if e := d.entries[key]; e.dir != nil {
			t.emit(e.dir)
		}
	}
	b := t.scratch[:0]
	for _, key := range d.order {
		e := d.entries[key]
		if e.dir != nil {
			b = append(b, "40000 "...)
			b = append(b, e.name...)
			b = append(b, 0)
			b = append(b, e.dir.raw[:]...)
		} else {
			b = append(b, "100644 "...)
			b = append(b, e.name...)
			b = append(b, 0)
			b = append(b, e.blob[:]...)
		}
	}
	t.scratch = b
	// Put copies the payload, so the scratch buffer can be reused.
	d.oid = t.objects.Put("tree", b)
	_, _ = hex.Decode(d.raw[:], []byte(d.oid))
	return d.oid
}

// readyQueue is a min-heap of positions in an eventCompare-sorted slice, so its
// minimum is always the earliest ready event.
type readyQueue []int

func (q readyQueue) Len() int           { return len(q) }
func (q readyQueue) Less(i, j int) bool { return q[i] < q[j] }
func (q readyQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *readyQueue) Push(x any)        { *q = append(*q, x.(int)) }
func (q *readyQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

func (q *readyQueue) push(i int) { heap.Push(q, i) }
func (q *readyQueue) pop() int   { return heap.Pop(q).(int) }
