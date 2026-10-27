package canonical

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Format 3 shows line starts and tags in readable form. Their messages are
// not parsed back: the generation is replayed from the source and compared
// byte for byte, so each message only has to tell the lines and tags apart.

// rawTagDir holds tags whose names cannot be ref names themselves. A plain
// tag never contains a slash, so nothing can collide with this directory.
const rawTagDir = "cvs-raw"

const tagPrefix = "refs/tags/"

// CVS tag names are letters, digits and a few punctuation characters.
var plainTagName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// TagRef returns the Git ref of a tag snapshot. Format 3 uses refs/tags/<name>
// whenever the name is a safe ref name and falls back to the hex encoding
// refs/tags/cvs-raw/<hex>; format 2 always uses refs/tags/cvs/b<hex>.
func (p Policy) TagRef(name string) (string, error) {
	if p.Version() != 3 {
		return EncodedRef("refs/tags/cvs/", name)
	}
	if plainTagName.MatchString(name) && name != rawTagDir && ValidBranchName(name) && len(tagPrefix)+len(name) <= MaxRef {
		return tagPrefix + name, nil
	}
	ref := tagPrefix + rawTagDir + "/" + Hex(name)
	if len(ref) > MaxRef {
		return "", fmt.Errorf("unsupported ref length")
	}
	return ref, nil
}

// IsTagRef reports whether ref belongs to the tag projections of this policy.
func (p Policy) IsTagRef(ref string) bool {
	if p.Version() == 3 {
		return strings.HasPrefix(ref, tagPrefix)
	}
	return strings.HasPrefix(ref, "refs/tags/cvs/")
}

// validTagRef accepts exactly the refs TagRef can produce under format 3.
func validTagRef(ref string) bool {
	name, ok := strings.CutPrefix(ref, tagPrefix)
	if !ok || name == "" {
		return false
	}
	if h, raw := strings.CutPrefix(name, rawTagDir+"/"); raw {
		b, err := hex.DecodeString(h)
		if err != nil || h == "" || Hex(string(b)) != h {
			return false
		}
		name = string(b)
	}
	got, err := Policy{Format: 3}.TagRef(name)
	return err == nil && got == ref
}

// tagMessage names the tag the commit snapshots.
func (p Policy) tagMessage(name string) string {
	return "CVS tag " + textField(name) + "\n"
}

// lineMessage is the format 3 message of the first commit of a project line,
// with the trunk as a root and every other line as a branch seed.
func (p Policy) lineMessage(line *projectLine) string {
	switch {
	case line.id == "trunk":
		return "CVS trunk\n"
	case strings.HasPrefix(line.id, "named\x00"):
		return "CVS branch " + textField(strings.TrimPrefix(line.id, "named\x00")) + "\n"
	default:
		f := line.members[0]
		return "CVS unnamed branch " + f.number + " of " + textField(f.archive) + "\n"
	}
}
