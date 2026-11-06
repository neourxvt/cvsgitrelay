// Package secret encodes and decodes the access secrets that the TOML
// configuration may hold, such as a token for pushing to a Git remote.
//
// This is obfuscation, not protection: the key is compiled into the program
// and visible in this source, as intended. It keeps a token from being read
// off a screen, a diff or a backup of the configuration by accident; anyone
// who can read the configuration and has this program or source can decode it.
// Real protection is the file permissions of the configuration.
package secret

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// Prefix marks an encoded secret and names its format version.
const Prefix = "xc1:"

// keyHex is the compiled-in 256-bit key. Changing it makes every secret
// encoded before unreadable.
const keyHex = "acafb143074587458c44039a2bfb94a490d64296ea33d5c2da2ca52120b324df"

// aadContext is authenticated with every secret, so that ciphertext from another
// use of the same key is not accepted as a secret of this program.
var aadContext = []byte("cvsgitrelay secret v1")

func newCipher() (cipher.AEAD, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.NewX(key)
}

// Encode seals plaintext with XChaCha20-Poly1305 under a random 24-byte nonce
// and returns "xc1:" followed by the unpadded base64url of nonce and
// ciphertext. The Poly1305 tag makes a damaged or edited secret fail to
// decode instead of yielding garbage. Encoding the same secret twice gives
// different results.
func Encode(plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("the secret is empty")
	}
	aead, err := newCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), aadContext)
	return Prefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// maxFile bounds the file that Load reads; encoded secrets are short.
const maxFile = 64 << 10

// Load returns the plain secret for a configuration value that is either an
// encoded secret ("xc1:...") or the path of a file that holds one. The file
// may end in a line break, as a file written with echo does. Errors never
// contain the value, because a plain secret pasted by mistake would look like
// a path.
func Load(value string) (string, error) {
	if strings.HasPrefix(value, Prefix) {
		return Decode(value)
	}
	f, err := os.Open(value)
	if err != nil {
		return "", errors.New("the value is neither an encoded secret (xc1:...) nor the path of a readable file; create one with \"cvsgitrelay encode-secret\"")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(data) > maxFile {
		return "", errors.New("the secret file cannot be read or is too large")
	}
	return Decode(strings.TrimSpace(string(data)))
}

// Decode reverses Encode. Its errors never contain the input.
func Decode(encoded string) (string, error) {
	body, ok := strings.CutPrefix(encoded, Prefix)
	if !ok {
		return "", errors.New("not an encoded secret; create one with \"cvsgitrelay encode-secret\"")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", errors.New("the encoded secret is damaged")
	}
	aead, err := newCipher()
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize()+1 {
		return "", errors.New("the encoded secret is damaged")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], aadContext)
	if err != nil {
		return "", errors.New("the encoded secret is damaged or was encoded by another build")
	}
	return string(plain), nil
}
