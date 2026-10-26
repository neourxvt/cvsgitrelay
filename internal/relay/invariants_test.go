package relay

import "testing"

func TestSourceLocatorsAndPaths(t *testing.T) {
	if compareNumber("1.9", "1.10") >= 0 {
		t.Fatal("revision ordering was lexicographic")
	}
	if b, ok := branchNumber("1.3.0.2"); !ok || b != "1.3.2" {
		t.Fatal("magic branch number was not normalized")
	}
	for _, root := range []string{"/repo", ":fork:/repo", ":ext:user@host:/repo", ":pserver:user@host:2401/repo", ":ext:user@[::1]:/repo"} {
		if got, err := sourceDirectory(root); err != nil || got != "/repo" {
			t.Fatalf("locator %q: %q %v", root, got, err)
		}
	}
	for _, root := range []string{":pserver:user:secret@host:/repo", ":ext:user@host:invalid/repo", "/repo/../escape", ":fork:relative"} {
		if _, err := sourceDirectory(root); err == nil {
			t.Fatal("unsafe locator accepted")
		}
	}
	for _, path := range []string{"../escape", "/absolute", "a/.git/config", "a\nnewline", "a\ttab", "a\x00nul"} {
		if err := validatePath(path); err == nil {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
	for _, path := range []string{"a space", "-leading-option", "caf\xc3\xa9", "nonutf8-\xff"} {
		if err := validatePath(path); err != nil {
			t.Fatal(err)
		}
	}
}
