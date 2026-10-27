package relay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
)

const trunkRef = "refs/heads/cvs/trunk"
const stateRef = "refs/cvsgitrelay/state"
const journalRef = "refs/cvsgitrelay/ops/publish"
const zeroOID = "0000000000000000000000000000000000000000"

// Local access settings are persisted for convenience but never hashed into
// policy or objects. Policy changes require a fresh destination.
type Config struct {
	Version  int              `json:"format_version"`
	SourceID string           `json:"source_id"`
	CVSRoot  string           `json:"cvsroot"`
	Module   string           `json:"module"`
	Policy   canonical.Policy `json:"policy"`
}

type Repository struct {
	mirroring                   bool
	Path                        string
	Config                      Config
	Runner                      *process.Runner
	StateOID                    string
	Manifest                    canonical.Manifest
	Submodules                  string
	RequireConsecutiveRevisions bool
	sourceReader                func(context.Context) (canonical.Inventory, sourceStats, error)
	// openedState marks the generation that Open validated, so the first
	// operation under the same lock need not repeat the full validation.
	openedState      bool
	objectWrites     int
	metadataRequests int
	metadataBytes    int64
	lock             *os.File
	// phases times the steps of the running sync or verify; nil otherwise.
	phases *phaseTimer
}

func (r *Repository) submodulePolicy() string {
	if r.Submodules != "" {
		return r.Submodules
	}
	if r.Config.Policy.Submodules != "" {
		return r.Config.Policy.Submodules
	}
	return "exclude"
}

// A transport alias is allowed; normalized archive identity and full source
// verification establish equivalence rather than hashing the CVSROOT locator.
func (r *Repository) ConfigureSource(root, module, submodules string) error {
	if module != r.Config.Module || submodules != r.Config.Policy.Submodules {
		return diag.New("config", "source.configure", "canonical scope policy differs; use a new destination", nil)
	}
	if _, err := sourceDirectory(root); err != nil {
		return err
	}
	r.Config.CVSRoot = root
	r.Runner.Root = root
	return nil
}

func (r *Repository) git(ctx context.Context, input []byte, env []string, args ...string) ([]byte, error) {
	return r.gitAt(ctx, "", input, env, args...)
}

// gitAt runs Git with a working directory; the repository is always selected
// through its absolute --git-dir.
func (r *Repository) gitAt(ctx context.Context, dir string, input []byte, env []string, args ...string) ([]byte, error) {
	prefix := []string{"--git-dir=" + r.Path, "-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "gc.auto=0", "-c", "core.fsync=loose-object,reference", "-c", "core.fsyncMethod=fsync", "-c", "maintenance.auto=false", "-c", "commit.gpgSign=false", "-c", "i18n.commitEncoding=UTF-8"}
	return r.Runner.Run(ctx, dir, env, input, "git", append(prefix, args...)...)
}

func validOID(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 20 && s == strings.ToLower(s)
}

// localCVSRoot reports whether CVS reads the repository from this machine's
// filesystem instead of through a network transport.
func localCVSRoot(root string) bool {
	return strings.HasPrefix(root, "/") || strings.HasPrefix(root, ":local:") || strings.HasPrefix(root, ":fork:")
}

func sourceDirectory(root string) (string, error) {
	if strings.ContainsAny(root, "\x00\r\n") || !utf8.ValidString(root) {
		return "", fmt.Errorf("CVSROOT contains incompatible bytes")
	}
	p := root
	if strings.HasPrefix(root, ":") {
		method, rest, ok := strings.Cut(root[1:], ":")
		if !ok || !slices.Contains([]string{"local", "fork", "ext", "pserver", "gserver", "kserver"}, method) {
			return "", fmt.Errorf("unsupported CVSROOT transport syntax")
		}
		if method == "local" || method == "fork" {
			p = rest
		} else {
			slash := strings.IndexByte(rest, '/')
			if slash < 0 {
				return "", fmt.Errorf("remote CVSROOT must contain an absolute repository directory")
			}
			authority := rest[:slash]
			colon := strings.LastIndexByte(authority, ':')
			if colon < 0 {
				return "", fmt.Errorf("remote CVSROOT must separate host and repository directory")
			}
			port := authority[colon+1:]
			for _, digit := range port {
				if digit < '0' || digit > '9' {
					return "", fmt.Errorf("unsupported CVSROOT port syntax")
				}
			}
			authority = authority[:colon]
			if at := strings.LastIndexByte(authority, '@'); at >= 0 && strings.Contains(authority[:at], ":") {
				return "", fmt.Errorf("credentials in CVSROOT are forbidden; use CVS authentication facilities")
			}
			p = rest[slash:]
		}
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		return "", fmt.Errorf("CVSROOT repository directory must be absolute and canonical")
	}
	return p, nil
}

func (r *Repository) validateBare(ctx context.Context) error {
	out, err := r.git(ctx, nil, nil, "rev-parse", "--is-bare-repository", "--show-object-format")
	if err != nil {
		return err
	}
	if string(out) != "true\nsha1\n" {
		return diag.New("unsupported", "repository.validate", "destination must be bare with SHA-1 objects", nil)
	}
	for _, name := range []string{"shallow", "info/grafts", "objects/info/alternates"} {
		if _, err := os.Stat(filepath.Join(r.Path, name)); !os.IsNotExist(err) {
			return diag.New("unsupported", "repository.validate", "shallow history, grafts and alternate object stores are unsupported", err, "file", name)
		}
	}
	return nil
}

func (r *Repository) acquire() error {
	f, err := os.OpenFile(filepath.Join(r.Path, "cvsgitrelay.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return diag.New("io", "lock.acquire", "open local writer lock", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return diag.New("locked", "lock.acquire", "another exporter is using this destination; retry after it exits", err)
	}
	r.lock = f
	return nil
}

func (r *Repository) Close() {
	if r.lock != nil {
		_ = syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN)
		_ = r.lock.Close()
		r.lock = nil
	}
}

func (r *Repository) allRefs(ctx context.Context) (map[string]string, error) {
	out, err := r.git(ctx, nil, nil, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 3 || !validOID(parts[1]) {
			return nil, diag.New("integrity", "refs.enumerate", "invalid Git ref record", nil)
		}
		if r.ownedRef(parts[0]) && parts[2] != "" {
			return nil, diag.New("conflict", "refs.enumerate", "symbolic managed refs are forbidden", nil, "ref", parts[0])
		}
		refs[parts[0]] = parts[1]
	}
	return refs, nil
}

func decodeJSON(data []byte, dst any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing or malformed JSON data")
	}
	return nil
}

// ownedRef reports whether the exporter owns a ref: its reserved namespaces
// and the Git branches named by the policy's branch map.
func (r *Repository) ownedRef(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/cvs/") || r.Config.Policy.IsTagRef(ref) || strings.HasPrefix(ref, "refs/cvsgitrelay/") || r.Config.Policy.MappedBranchRef(ref)
}

// Format 2 destinations keep their hex revision events; new destinations use
// format 3. The configuration and policy must agree on the format.
func validateConfig(c Config) error {
	if (c.Version != 2 && c.Version != 3) || c.Version != c.Policy.Version() {
		return diag.New("unsupported", "config.validate", "unsupported destination format", nil)
	}
	if c.SourceID != c.Policy.Source || c.Module != c.Policy.Module {
		return diag.New("config", "config.validate", "inconsistent canonical identity", nil)
	}
	if err := c.Policy.Validate(); err != nil {
		return diag.New("config", "config.validate", "invalid canonical policy", err)
	}
	_, err := sourceDirectory(c.CVSRoot)
	return err
}

// Init reserves only an empty dedicated destination. Initialization stores the
// selected policy but does not publish a generation before a full source scan.
// New destinations use format 3; authors is its optional CVS username map and
// branches the optional selection and renaming of branches.
func Init(ctx context.Context, runner *process.Runner, path, root, module, source, submodules, verification string, authors, branches map[string]string) error {
	r := &Repository{Config: Config{Version: 3, SourceID: source, CVSRoot: root, Module: module}, Runner: runner, Submodules: submodules}
	if _, err := sourceDirectory(root); err != nil {
		return err
	}
	if err := modulePath(module); err != nil {
		return err
	}
	initialPolicy := canonical.Policy{Source: source, Module: module, Submodules: submodules, Verify: verification, Mappings: []canonical.Mapping{{Archive: module}}, Format: 3, Authors: authors, Branches: branches}
	if err := initialPolicy.Validate(); err != nil {
		return diag.New("config", "init.policy", "invalid canonical policy", err)
	}
	layout, _, err := r.moduleLayout(ctx)
	if err != nil {
		return err
	}

	policy := canonical.Policy{Source: source, Module: module, Submodules: submodules, Verify: verification, Format: 3, Authors: authors, Branches: branches}
	for _, m := range layout {
		policy.Mappings = append(policy.Mappings, canonical.Mapping{Archive: m.Source, Checkout: m.Destination})
	}
	r.Config.Policy = policy
	if err := validateConfig(r.Config); err != nil {
		return err
	}
	r.Path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.Path, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(r.Path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if _, err := runner.Run(ctx, "", nil, nil, "git", "-c", "core.hooksPath=/dev/null", "init", "--bare", "--object-format=sha1", "--ref-format=reftable", "--template=", r.Path); err != nil {
			return err
		}
	}
	if err := r.acquire(); err != nil {
		return err
	}
	defer r.Close()
	if _, err := os.Stat(filepath.Join(r.Path, "cvsgitrelay.json")); !os.IsNotExist(err) {
		return diag.New("config", "init", "destination already configured", err)
	}
	if err := r.validateBare(ctx); err != nil {
		return err
	}
	refs, err := r.allRefs(ctx)
	if err != nil {
		return err
	}
	if len(refs) != 0 {
		return diag.New("conflict", "init", "destination must be an empty dedicated bare repository", nil)
	}
	// Hex-encoded internal line IDs can exceed a filesystem component limit.
	// Reftable keeps the supported 4096-byte ref limit independent of paths.
	format, err := r.git(ctx, nil, nil, "rev-parse", "--show-ref-format")
	if err != nil {
		return err
	}
	if string(format) != "reftable\n" {
		if _, err := r.git(ctx, nil, nil, "refs", "migrate", "--ref-format=reftable"); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(r.Config, "", "  ")
	if err != nil {
		return err
	}
	if _, err := r.git(ctx, nil, nil, "symbolic-ref", "HEAD", r.Config.Policy.HeadRef()); err != nil {
		return err
	}
	return durableFile(filepath.Join(r.Path, "cvsgitrelay.json"), append(data, '\n'))
}

// Open validates the destination and completes pending publication before use.
func Open(ctx context.Context, runner *process.Runner, path string) (*Repository, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	r := &Repository{Path: abs, Runner: runner}
	if err := r.acquire(); err != nil {
		return nil, err
	}
	fail := func(err error) (*Repository, error) { r.Close(); return nil, err }
	info, err := os.Stat(filepath.Join(abs, "cvsgitrelay.json"))
	if err != nil {
		return fail(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fail(diag.New("config", "repository.open", "cvsgitrelay.json must have mode 0600", nil))
	}
	data, err := os.ReadFile(filepath.Join(abs, "cvsgitrelay.json"))
	if err != nil {
		return fail(err)
	}
	if err := decodeJSON(data, &r.Config); err != nil {
		return fail(err)
	}
	if err := validateConfig(r.Config); err != nil {
		return fail(err)
	}
	runner.Root = r.Config.CVSRoot
	if err := r.validateBare(ctx); err != nil {
		return fail(err)
	}
	format, err := r.git(ctx, nil, nil, "rev-parse", "--show-ref-format")
	if err != nil {
		return fail(err)
	}
	if string(format) != "reftable\n" {
		return fail(diag.New("unsupported", "repository.refs", "destinations require their initialized reftable backend", nil))
	}
	if err := r.recoverPublication(ctx); err != nil {
		return fail(err)
	}
	if err := r.loadCurrent(ctx); err != nil {
		return fail(err)
	}
	r.openedState = true
	return r, nil
}
