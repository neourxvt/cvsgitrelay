package main

import (
	"bytes"
	"strings"
	"testing"

	"cvsgitrelay/internal/secret"
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
