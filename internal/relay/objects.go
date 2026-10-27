package relay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

const emptyTreeOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Batches bound per-process stdin, staged payload files and response sizes
// while replacing one Git subprocess per object with a handful per generation.
const (
	probeBatchObjects = 1 << 16
	writeBatchObjects = 1 << 14
	writeBatchBytes   = 64 << 20
)

// objectStagingPrefix names temporary payload directories inside the bare
// repository; leftovers from an interrupted writer are removed under the lock.
const objectStagingPrefix = ".cvsgitrelay-objects-"

// batchRows caps how many fixed-size response rows one batch may request, so
// line-oriented Git output also stays inside the subprocess output limit.
func (r *Repository) batchRows(limit, rowBytes int) int {
	return max(1, min(limit, r.Runner.MaxOutput/2/rowBytes))
}

type objectInfo struct {
	kind string
	size int
}

// probeObjects reports the type and size of every present object. Missing
// objects are absent from the result.
func (r *Repository) probeObjects(ctx context.Context, oids []string) (map[string]objectInfo, error) {
	info := map[string]objectInfo{}
	// A row is an ID, a type name and a decimal size.
	rows := r.batchRows(probeBatchObjects, 80)
	for start := 0; start < len(oids); start += rows {
		chunk := oids[start:min(start+rows, len(oids))]
		out, err := r.git(ctx, []byte(strings.Join(chunk, "\n")+"\n"), nil, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != len(chunk) {
			return nil, diag.New("integrity", "objects.probe", "invalid object probe", nil)
		}
		for i, row := range lines {
			fields := strings.Split(row, " ")
			if len(fields) == 2 && fields[0] == chunk[i] && fields[1] == "missing" {
				continue
			}
			if len(fields) != 3 || fields[0] != chunk[i] {
				return nil, diag.New("integrity", "objects.probe", "invalid object probe", nil)
			}
			size, err := strconv.Atoi(fields[2])
			if err != nil || size < 0 {
				return nil, diag.New("integrity", "objects.probe", "invalid object probe", err)
			}
			info[chunk[i]] = objectInfo{fields[1], size}
		}
	}
	return info, nil
}

// readObjects streams exact payloads through `cat-file --batch`. Requests are
// grouped by their probed sizes so every response stays well inside the
// subprocess output limit. visit must not retain data after it returns.
func (r *Repository) readObjects(ctx context.Context, oids []string, visit func(oid, kind string, data []byte) error) error {
	info, err := r.probeObjects(ctx, oids)
	if err != nil {
		return err
	}
	for _, oid := range oids {
		if _, ok := info[oid]; !ok {
			return diag.New("integrity", "objects.read", "expected object is missing", nil, "oid", oid)
		}
	}
	budget := max(r.Runner.MaxOutput/2, 1)
	for start := 0; start < len(oids); {
		end, size := start, 0
		// Each response row adds a header and a newline to the payload.
		for end < len(oids) && (end == start || size+info[oids[end]].size+64 <= budget) {
			size += info[oids[end]].size + 64
			end++
		}
		chunk := oids[start:end]
		out, err := r.git(ctx, []byte(strings.Join(chunk, "\n")+"\n"), nil, "cat-file", "--batch", "--buffer")
		if err != nil {
			return err
		}
		for _, oid := range chunk {
			newline := bytes.IndexByte(out, '\n')
			if newline < 0 {
				return diag.New("integrity", "objects.read", "truncated object batch", nil)
			}
			fields := strings.Split(string(out[:newline]), " ")
			out = out[newline+1:]
			want := info[oid]
			if len(fields) != 3 || fields[0] != oid || fields[1] != want.kind || fields[2] != strconv.Itoa(want.size) || len(out) < want.size+1 || out[want.size] != '\n' {
				return diag.New("integrity", "objects.read", "invalid object batch record", nil, "oid", oid)
			}
			if err := visit(oid, want.kind, out[:want.size]); err != nil {
				return err
			}
			out = out[want.size+1:]
		}
		if len(out) != 0 {
			return diag.New("integrity", "objects.read", "unexpected trailing object batch data", nil)
		}
		start = end
	}
	return nil
}

// writeObjects stores payloads of one kind through `hash-object --stdin-paths`
// instead of one process per object. --no-filters keeps blobs byte-exact even if
// repository configuration names conversions. Batch fsync still makes every
// object durable before the command exits.
func (r *Repository) writeObjects(ctx context.Context, kind string, oids []string, objects canonical.Objects) error {
	dir, err := os.MkdirTemp(r.Path, objectStagingPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	names := make([]string, len(oids))
	for i, oid := range oids {
		names[i] = strconv.Itoa(i)
		if err := os.WriteFile(filepath.Join(dir, names[i]), objects[oid].Data, 0600); err != nil {
			return err
		}
	}
	// Relative names keep stdin unambiguous regardless of the repository path.
	out, err := r.gitAt(ctx, dir, []byte(strings.Join(names, "\n")+"\n"), nil, "-c", "core.fsyncMethod=batch", "hash-object", "-t", kind, "-w", "--no-filters", "--stdin-paths")
	if err != nil {
		return err
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(oids) {
		return diag.New("integrity", "objects.write", "invalid batch object write result", nil)
	}
	for i, oid := range oids {
		if got[i] != oid {
			return diag.New("integrity", "objects.write", "Git disagrees with canonical object encoder", nil)
		}
	}
	return nil
}

// removeObjectStaging deletes payload directories left by an interrupted run.
// Callers hold the writer lock, so no other exporter can still be using them.
func (r *Repository) removeObjectStaging() error {
	entries, err := os.ReadDir(r.Path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), objectStagingPrefix) {
			if err := os.RemoveAll(filepath.Join(r.Path, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// packObjects moves newly written reachable loose objects into one incremental
// pack. It never deletes unreachable objects or existing packs, so readers that
// resolved an older generation keep working. Pack files are fsynced before
// prune-packed removes the loose copies.
func (r *Repository) packObjects(ctx context.Context) error {
	_, err := r.git(ctx, nil, nil, "-c", "core.fsync=loose-object,reference,pack,pack-metadata", "repack", "-d", "-q", "--no-write-bitmap-index")
	return err
}
