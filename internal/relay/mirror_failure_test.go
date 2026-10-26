package relay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
)

// A final metadata failure must preserve the expensive downloads and generated
// archives, including across another failed attempt, so a retry can reuse them.
func TestMirrorRetainsWorkOnFinalComparisonFailure(t *testing.T) {
	root, _, _ := fixture(t)
	cvs, err := exec.LookPath("cvs")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "mirror")
	failure := filepath.Join(t.TempDir(), "fail")
	writeFixture(t, failure, nil)
	bin := t.TempDir()
	// Only alter the final local inventory, after all revision verification and
	// downloads have completed. The source and generated archives stay intact.
	wrapper := `#!/bin/sh
local_scan=false
rlog=false
for argument do
    case "$argument" in
        "$CVSGITRELAY_TEST_OUTPUT"/.cvsgitrelay-mirror/work-*) local_scan=true ;;
        rlog) rlog=true ;;
    esac
done
if "$local_scan" && "$rlog" && test -f "$CVSGITRELAY_TEST_FAILURE"; then
    "$CVSGITRELAY_TEST_CVS" "$@" | sed 's/^keyword substitution: kv$/keyword substitution: b/'
else
    exec "$CVSGITRELAY_TEST_CVS" "$@"
fi
`
	if err := os.WriteFile(filepath.Join(bin, "cvs"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CVSGITRELAY_TEST_CVS", cvs)
	t.Setenv("CVSGITRELAY_TEST_OUTPUT", output)
	t.Setenv("CVSGITRELAY_TEST_FAILURE", failure)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := &process.Runner{Log: process.DiscardLogger(), Timeout: 10 * time.Second, MaxOutput: 16 << 20}
	options := Options{Connections: 2}
	run := func() (MirrorSummary, error) {
		return Mirror(context.Background(), runner, ":fork:"+root, "module", output, false, options)
	}
	var retained map[string]string
	for attempt := 0; attempt < 2; attempt++ {
		_, err := run()
		if diag.Code(err) != "integrity" || !strings.Contains(err.Error(), "generated CVS history differs") {
			t.Fatalf("expected final comparison failure, got %v", err)
		}
		if !strings.Contains(err.Error(), "retained") {
			t.Fatalf("failure does not explain retained output: %v", err)
		}
		for path, data := range retained {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatalf("retry discarded retained work %q: %v", path, err)
			}
		}
		if attempt == 0 {
			retained = map[string]string{}
			for _, pattern := range []string{"work-*/module/*,v", "work-tree-*/tree/*", "blobs/*", "revisions/*"} {
				paths, err := filepath.Glob(filepath.Join(output, mirrorStateDir, pattern))
				if err != nil || len(paths) == 0 {
					t.Fatalf("missing retained %s: %v", pattern, err)
				}
				for _, path := range paths {
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if info.IsDir() {
						continue
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					retained[path] = string(data)
				}
			}
		}
	}
	if err := os.Remove(failure); err != nil {
		t.Fatal(err)
	}
	summary, err := run()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Downloaded != 0 || summary.Reused != summary.Revisions {
		t.Fatalf("retry did not reuse all historical downloads: %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(output, "module", "active,v")); err != nil {
		t.Fatalf("retry did not publish the retained history: %v", err)
	}
	work, err := filepath.Glob(filepath.Join(output, mirrorStateDir, "work-*"))
	if err != nil || len(work) != 0 {
		t.Fatalf("successful publication did not clean derived work: %v %v", work, err)
	}
}
