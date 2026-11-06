package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, plain := range []string{"ghp_abc123", "x", "ünïcode token\twith spaces", strings.Repeat("a", 4096)} {
		a, err := Encode(plain)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := Encode(plain)
		if a == b {
			t.Error("two encodings are identical; the nonce is not random")
		}
		if len(plain) > 5 && strings.Contains(a, plain) {
			t.Error("the encoding contains the plaintext")
		}
		got, err := Decode(a)
		if err != nil || got != plain {
			t.Errorf("round trip of %.10q: %q, %v", plain, got, err)
		}
	}
	if _, err := Encode(""); err == nil {
		t.Error("an empty secret was encoded")
	}
}

// Anything but an intact encoding fails, without echoing the input.
func TestDecodeRejects(t *testing.T) {
	good, _ := Encode("ghp_secret")
	flipped := []byte(good)
	flipped[len(flipped)-3] ^= 1
	for name, bad := range map[string]string{
		"plain":     "ghp_secret",
		"empty":     "",
		"prefix":    Prefix,
		"short":     Prefix + "AAAA",
		"not b64":   Prefix + "!!!!",
		"tampered":  string(flipped),
		"truncated": good[:len(good)-4],
	} {
		_, err := Decode(bad)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("%s: the error repeats the input", name)
		}
	}
}

// Load takes an encoded secret or a file holding one, and never repeats a
// value that is neither, since a plain secret pasted by mistake looks like a path.
func TestLoad(t *testing.T) {
	encoded, _ := Encode("ghp_token")
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, value := range map[string]string{
		"inline":  encoded,
		"file":    write("a", encoded),
		"newline": write("b", encoded+"\n"),
		"crlf":    write("c", "  "+encoded+"\r\n"),
	} {
		if got, err := Load(value); err != nil || got != "ghp_token" {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	for name, value := range map[string]string{
		"plain text": "ghp_token_pasted_by_mistake",
		"missing":    filepath.Join(dir, "missing"),
		"plain file": write("d", "ghp_token"),
		"empty file": write("e", ""),
		"directory":  dir,
		"huge file":  write("f", strings.Repeat("a", maxFile+1)),
	} {
		_, err := Load(value)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), value) {
			t.Errorf("%s: the error repeats the value", name)
		}
	}
}
