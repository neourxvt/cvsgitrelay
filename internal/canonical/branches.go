package canonical

import (
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
// empty map keeps the default projection of every branch under refs/heads/cvs/.
func (p Policy) BranchMapped() bool { return len(p.Branches) > 0 }

// BranchRef returns the Git ref of a CVS branch under the branch map and
// whether the branch is selected. Without a map the default encoding applies.
func (p Policy) BranchRef(name string) (string, bool, error) {
	if !p.BranchMapped() {
		if name == TrunkBranch {
			return "refs/heads/cvs/trunk", true, nil
		}
		ref, err := EncodedRef("refs/heads/cvs/", name)
		return ref, true, err
	}
	target, ok := p.Branches[name]
	return branchPrefix + target, ok, nil
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

// HeadRef is the branch a clone checks out: the mapped trunk, or the first
// mapped branch by Git branch name if the trunk is not selected.
func (p Policy) HeadRef() string {
	if !p.BranchMapped() {
		return "refs/heads/cvs/trunk"
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
	return parseManifest(b, func(ref string) bool { return ProjectionRef(ref) || p.MappedBranchRef(ref) })
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
