package relay

import (
	"context"
	"strings"
	"time"
)

// supersededPrefix holds the generations replaced by a sync that accepted
// changed or removed CVS history. Each ref names an old state commit, which
// keeps that whole generation reachable, so neither the new generation nor
// GC can lose history that CVS no longer has. The relay never moves or
// deletes these refs; an operator may delete them once they are not needed.
const supersededPrefix = "refs/cvsgitrelay/superseded/"

// supersede keeps the current generation under a superseded ref before a
// generation without some of its history replaces it. The name holds the UTC
// time for readers and the start of the state ID, so that no two refs can
// collide. A rerun after an interrupted publication reuses the ref already
// naming the generation.
func (r *Repository) supersede(ctx context.Context, now time.Time) (string, error) {
	refs, err := r.allRefs(ctx)
	if err != nil {
		return "", err
	}
	for ref, oid := range refs {
		if strings.HasPrefix(ref, supersededPrefix) && oid == r.StateOID {
			return ref, nil
		}
	}
	ref := supersededPrefix + now.UTC().Format("20060102T150405Z") + "-" + r.StateOID[:12]
	if err := r.cas(ctx, ref, "", r.StateOID); err != nil {
		return "", err
	}
	return ref, nil
}

// changeList describes at most limit history changes for the log.
func changeList(changes []historyChange, limit int) []string {
	var list []string
	for _, c := range changes[:min(len(changes), limit)] {
		kind := "changed"
		if c.Removed {
			kind = "removed"
		}
		list = append(list, c.Key.Archive+" "+c.Key.Number+" "+kind)
	}
	return list
}
