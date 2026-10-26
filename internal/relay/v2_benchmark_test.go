package relay

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The fixture is intentionally small and reproducible. Use -benchtime=1x so
// every evolution case measures one import of exactly the documented state.
func BenchmarkV2Sync(b *testing.B) {
	for _, scenario := range []string{"Fresh", "NoChange", "OneRevision", "EarlierArchive", "BranchMember"} {
		b.Run(scenario, func(b *testing.B) {
			root, work, _ := fixture(b)
			normalizeBenchmarkSource(b, root)
			r := openFixture(b, root, "full")
			ctx := context.Background()
			if scenario != "Fresh" {
				if _, err := r.Sync(ctx, Options{}); err != nil {
					b.Fatal(err)
				}
			}
			switch scenario {
			case "OneRevision":
				writeFixture(b, filepath.Join(work, "active"), []byte("new branch revision\n"))
				fixtureCVS(b, root, work, "commit", "-m", "new branch revision", "active")
				normalizeBenchmarkSource(b, root)
			case "EarlierArchive":
				// An explicitly linked, genuinely new earlier-sorting revision.
				writeFixture(b, filepath.Join(root, "module", "earlier,v"), []byte("head 1.1;\naccess;\nsymbols;\nlocks; strict;\n1.1\ndate 2000.01.01.00.00.00; author a; state Exp;\nbranches;\nnext;\ndesc\n@@\n1.1\nlog\n@earlier@\ntext\n@earlier\\n@\n"))
			case "BranchMember":
				writeFixture(b, filepath.Join(work, "late"), []byte("late branch file\n"))
				fixtureCVS(b, root, work, "add", "late")
				fixtureCVS(b, root, work, "commit", "-m", "late member", "late")
				normalizeBenchmarkSource(b, root)
			}
			b.ReportAllocs()
			b.ResetTimer()
			var summary Summary
			for i := 0; i < b.N; i++ {
				var err error
				summary, err = r.Sync(ctx, Options{})
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(summary.Discovered), "revisions")
			b.ReportMetric(float64(summary.SourceBytes), "source-bytes/op")
			b.ReportMetric(float64(summary.SourceRequests), "source-reads/op")
			b.ReportMetric(float64(summary.ObjectWrites), "new-objects/op")
		})
	}
}

// Benchmark inputs use fixed authors and dates instead of the account and clock
// used by the live-CVS capability fixture. Only RCS delta headers are rewritten.
func normalizeBenchmarkSource(b *testing.B, root string) {
	date := regexp.MustCompile(`date[\t ]+[0-9.]+;`)
	author := regexp.MustCompile(`author[\t ]+[^;]+;`)
	err := filepath.WalkDir(filepath.Join(root, "module"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ",v") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		at := bytes.Index(data, []byte("\ndesc\n"))
		if at < 0 {
			b.Fatal("fixture RCS description missing")
		}
		head := date.ReplaceAll(data[:at], []byte("date 2000.01.02.00.00.00;"))
		head = author.ReplaceAll(head, []byte("author fixture;"))
		data = append(head, data[at:]...)
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
		return os.WriteFile(path, data, 0600)
	})
	if err != nil {
		b.Fatal(err)
	}
}
