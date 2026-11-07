package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cvsgitrelay/internal/secret"
	"cvsgitrelay/internal/settings"
)

// Schedulers rely on 75 meaning "run again later" and on every permanent
// failure staying outside it.
func TestExitCodes(t *testing.T) {
	for code, want := range map[string]int{
		"retry": 75, "locked": 75, "timeout": 75,
		"source": 5, "parse": 5, "integrity": 5,
		"conflict": 6, "subprocess": 7, "limit": 7,
		"config": 3, "io": 74, "internal": 70,
	} {
		if got := exitCode(code); got != want {
			t.Errorf("%s: exit %d, want %d", code, got, want)
		}
	}
}

// encode-secret reads the secret from stdin, drops the final line break and
// prints something decode accepts, without echoing the secret.
func TestEncodeSecretCommand(t *testing.T) {
	for _, input := range []string{"ghp_token\n", "ghp_token\r\n", "ghp_token"} {
		var stdout, stderr bytes.Buffer
		cmd := newRootCommand(func(int) {})
		cmd.SetIn(strings.NewReader(input))
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"encode-secret"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		encoded := strings.TrimSpace(stdout.String())
		if got, err := secret.Decode(encoded); err != nil || got != "ghp_token" {
			t.Errorf("%q decoded to %q, %v", input, got, err)
		}
		if strings.Contains(stdout.String()+stderr.String(), "ghp_token") {
			t.Error("the output repeats the secret")
		}
	}
	cmd := newRootCommand(func(int) {})
	cmd.SetIn(strings.NewReader("\n"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"encode-secret"})
	if err := cmd.Execute(); err == nil {
		t.Error("an empty secret was encoded")
	}
}

// A secret file on the command line belongs to the caller's directory.
func TestCallerSecretPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := secret.Encode("tok")
	for value, want := range map[string]string{
		"":              "",
		"rel.secret":    filepath.Join(wd, "rel.secret"),
		"/abs/x.secret": "/abs/x.secret",
		encoded:         encoded,
	} {
		if got := callerSecretPath(value); got != want {
			t.Errorf("%q: %q, want %q", value, got, want)
		}
	}
}

// A scheduler that retries 75 quietly must still hear about a repository that
// needs a person, whatever the order.
func TestCombineExit(t *testing.T) {
	for want, codes := range map[int][]int{
		0: nil, 75: {0, 75, 0}, 5: {75, 5, 0}, 7: {7, 75}, 3: {0, 3, 7},
	} {
		if got := combineExit(codes); got != want {
			t.Errorf("%v: %d, want %d", codes, got, want)
		}
	}
	if got := combineExit([]int{0, 0}); got != 0 {
		t.Errorf("all succeeded: %d", got)
	}
}

func TestSelectRepositories(t *testing.T) {
	all := []settings.Config{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got, err := selectRepositories(all, []string{"c", "a"})
	if err != nil || len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("selection keeps file order: %v, %v", got, err)
	}
	if got, err := selectRepositories(all, nil); err != nil || len(got) != 3 {
		t.Fatalf("no selection: %v, %v", got, err)
	}
	if _, err := selectRepositories(all, []string{"x"}); err == nil {
		t.Error("an unknown name was accepted")
	}
	if _, err := selectRepositories([]settings.Config{{}}, []string{"a"}); err == nil {
		t.Error("--only was accepted for a single-repository file")
	}
}
