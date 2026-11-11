package canonical

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// TrunkBranch is the key of the CVS trunk in a branch map. Every other key is
// a raw CVS branch name.
const TrunkBranch = "trunk"

const branchPrefix = "refs/heads/"

// BranchMapped reports whether the policy selects and renames branches. An
// empty map keeps the default projection of every branch: under
// refs/heads/cvs/, or by readable name with ReadableBranches.
func (p Policy) BranchMapped() bool { return len(p.Branches) > 0 }

// BranchRef returns the Git ref of a CVS branch under the branch map and
// whether the branch is selected. Without a map the default encoding applies.
// The name "trunk" denotes the trunk.
func (p Policy) BranchRef(name string) (string, bool, error) {
	return p.branchRef(name, name == TrunkBranch)
}

// branchRef is BranchRef for a name known to denote the trunk or not, so that
// readable names can tell the trunk from a CVS branch literally named trunk.
func (p Policy) branchRef(name string, trunk bool) (string, bool, error) {
	switch {
	case p.BranchMapped():
		target, ok := p.Branches[name]
		return branchPrefix + target, ok, nil
	case p.ReadableBranches && trunk:
		return branchPrefix + TrunkBranch, true, nil
	case p.ReadableBranches:
		ref, err := readableBranchRef(name)
		return ref, true, err
	case name == TrunkBranch:
		return "refs/heads/cvs/trunk", true, nil
	default:
		ref, err := EncodedRef("refs/heads/cvs/", name)
		return ref, true, err
	}
}

// readableBranchRef returns refs/heads/<name> for a CVS branch whose name is a
// safe ref name, and refs/heads/cvs-raw/<hex name> otherwise. The ref depends
// on the name alone, so a new branch never renames an existing one. Plain
// names contain no slash, which keeps them apart from the cvs-raw directory;
// that directory's own name, the trunk's and HEAD are reserved.
func readableBranchRef(name string) (string, error) {
	if plainTagName.MatchString(name) && name != rawTagDir && name != TrunkBranch && name != "HEAD" && ValidBranchName(name) {
		return branchPrefix + name, nil
	}
	ref := branchPrefix + rawTagDir + "/" + Hex(name)
	if len(ref) > MaxRef {
		return "", fmt.Errorf("unsupported ref length")
	}
	return ref, nil
}

// validReadableBranchRef accepts exactly the refs readable naming can produce.
func validReadableBranchRef(ref string) bool {
	name, ok := strings.CutPrefix(ref, branchPrefix)
	if !ok || name == "" {
		return false
	}
	if name == TrunkBranch {
		return true
	}
	if h, raw := strings.CutPrefix(name, rawTagDir+"/"); raw {
		b, err := hex.DecodeString(h)
		if err != nil || h == "" || Hex(string(b)) != h {
			return false
		}
		name = string(b)
	}
	got, err := readableBranchRef(name)
	return err == nil && got == ref
}

// MappedBranchRef reports whether ref is a branch ref this policy's map owns.
func (p Policy) MappedBranchRef(ref string) bool {
	target, ok := strings.CutPrefix(ref, branchPrefix)
	if !ok {
		return false
	}
	for _, t := range p.Branches {
		if t == target {
			return true
		}
	}
	return false
}

// IsBranchRef reports whether ref belongs to the branch projections of this
// policy. Readable names own all of refs/heads/, as format 3 tags own all of
// refs/tags/, so a branch pushed there by hand counts as an unexpected ref.
func (p Policy) IsBranchRef(ref string) bool {
	switch {
	case p.BranchMapped():
		return p.MappedBranchRef(ref)
	case p.ReadableBranches:
		return strings.HasPrefix(ref, branchPrefix)
	default:
		return strings.HasPrefix(ref, "refs/heads/cvs/")
	}
}

// HeadRef is the branch a clone checks out: the trunk, or the first mapped
// branch by Git branch name if a map does not select the trunk.
func (p Policy) HeadRef() string {
	if !p.BranchMapped() {
		ref, _, _ := p.branchRef(TrunkBranch, true)
		return ref
	}
	if t, ok := p.Branches[TrunkBranch]; ok {
		return branchPrefix + t
	}
	targets := make([]string, 0, len(p.Branches))
	for _, t := range p.Branches {
		targets = append(targets, t)
	}
	slices.Sort(targets)
	return branchPrefix + targets[0]
}

// ParseManifest is ParseManifest for manifests produced under this policy,
// whose projections may include mapped branch refs.
func (p Policy) ParseManifest(b []byte) (Manifest, error) {
	return parseManifest(b, func(ref string) bool {
		return ProjectionRef(ref) || p.MappedBranchRef(ref) || (p.Version() == 3 && validTagRef(ref)) || (p.ReadableBranches && validReadableBranchRef(ref))
	})
}

// ValidBranchName applies the rules of git check-ref-format to a branch name
// that will live at refs/heads/<name>, and additionally rejects names that
// are not valid UTF-8 or begin with a dash.
func ValidBranchName(name string) bool {
	if name == "" || len(branchPrefix)+len(name) > MaxRef || !utf8.ValidString(name) || name == "@" ||
		strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "//") ||
		strings.Contains(name, "@{") || strings.ContainsAny(name, " ~^:?*[\\\x7f") {
		return false
	}
	for _, c := range []byte(name) {
		if c < 0x20 {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// ValidateBranchMap checks a branch map before it joins a policy.
func ValidateBranchMap(branches map[string]string) error {
	return Policy{Branches: branches}.validateBranches()
}

// validateBranches checks the map's keys and targets. Targets must be distinct
// valid names, and none may be a directory prefix of another, because Git
// cannot store both refs/heads/a and refs/heads/a/b.
func (p Policy) validateBranches() error {
	targets := make([]string, 0, len(p.Branches))
	for name, target := range p.Branches {
		if name == "" || len(name) > MaxAuthor || !utf8.ValidString(name) {
			return fmt.Errorf("branch map CVS names must contain 1..%d bytes of UTF-8", MaxAuthor)
		}
		if !ValidBranchName(target) {
			return fmt.Errorf("branch map target %q is not a valid Git branch name", target)
		}
		targets = append(targets, target)
	}
	slices.Sort(targets)
	for i := 1; i < len(targets); i++ {
		if targets[i] == targets[i-1] {
			return fmt.Errorf("branch map target %q is used more than once", targets[i])
		}
	}
	for _, a := range targets {
		for _, b := range targets {
			if strings.HasPrefix(b, a+"/") {
				return fmt.Errorf("branch map targets %q and %q cannot both exist in Git", a, b)
			}
		}
	}
	return nil
}
