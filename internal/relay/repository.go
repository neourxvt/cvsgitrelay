package relay

import (
	"context"
	"crypto/rand"
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

	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
)

const trunkRef = "refs/heads/cvs/trunk"
const stateRef = "refs/cvsgitrelay/state"
const zeroOID = "0000000000000000000000000000000000000000"

func namedRef(name string) string { return "refs/heads/cvs/branches/" + hexBytes(name) }
func archiveRef(path, line string) string {
	return "refs/cvsgitrelay/unmapped/" + hexBytes(path) + "/" + line
}

type Config struct {
	Version     int    `json:"format_version"`
	SourceID    string `json:"source_id"`
	CVSRoot     string `json:"cvsroot"`
	Module      string `json:"module"`
	KeywordMode string `json:"keyword_mode"`
	FileMode    string `json:"file_mode"`
	Scope       string `json:"scope"`
}

// Manifest is published as Git history in the same ref transaction as each
// import. It witnesses missing/rewound refs without an external state database.
type Manifest struct {
	Version    int                          `json:"version"`
	ConfigHash string                       `json:"config_hash"`
	Refs       map[string]string            `json:"refs"`
	Members    map[string]map[string]string `json:"members"` // path keys are hex
}

type Repository struct {
	Path     string
	Config   Config
	Runner   *process.Runner
	StateOID string
	Manifest Manifest
	lock     *os.File
}

func (r *Repository) git(ctx context.Context, input []byte, env []string, args ...string) ([]byte, error) {
	prefix := []string{"--git-dir=" + r.Path, "-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "commit.gpgSign=false"}
	return r.Runner.Run(ctx, "", env, input, "git", append(prefix, args...)...)
}

func (r *Repository) oid(ctx context.Context, input []byte, env []string, args ...string) (string, error) {
	out, err := r.git(ctx, input, env, args...)
	if err != nil {
		return "", err
	}
	s := strings.TrimSuffix(string(out), "\n")
	if !validOID(s) {
		return "", diag.New("integrity", "git.oid", "invalid SHA-1 object ID from Git", nil, "operation", args[0], "output_bytes", len(out))
	}
	return s, nil
}

func validOID(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 20 && s == strings.ToLower(s)
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
			idx := strings.Index(rest, ":/")
			if idx < 0 {
				return "", fmt.Errorf("remote CVSROOT must contain an absolute repository directory")
			}
			authority := rest[:idx]
			if at := strings.LastIndexByte(authority, '@'); at >= 0 && strings.Contains(authority[:at], ":") {
				return "", fmt.Errorf("credentials in CVSROOT are forbidden; use CVS authentication facilities")
			}
			p = rest[idx+1:]
		}
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		return "", fmt.Errorf("CVSROOT repository directory must be absolute and canonical")
	}
	return p, nil
}

func validateConfig(c Config) error {
	if c.Version != 1 || len(c.SourceID) != 32 || c.KeywordMode != "kb" || c.FileMode != "100644" || c.Scope != "all" {
		return diag.New("config", "config.validate", "incompatible immutable configuration; restore the initialized settings", nil)
	}
	if _, err := hex.DecodeString(c.SourceID); err != nil {
		return diag.New("config", "config.validate", "invalid source ID", nil)
	}
	if _, err := sourceDirectory(c.CVSRoot); err != nil {
		return diag.New("config", "config.validate", err.Error(), nil)
	}
	if err := validatePath(c.Module); err != nil {
		return err
	}
	if !utf8.ValidString(c.Module) {
		return diag.New("config", "config.validate", "module must be UTF-8; individual file paths may contain other POSIX bytes", nil)
	}
	return nil
}

func (r *Repository) configHash() string { b, _ := json.Marshal(r.Config); return digest(b) }

func Init(ctx context.Context, runner *process.Runner, path, root, module string) error {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return diag.New("io", "init", "generate source identity", err)
	}
	c := Config{Version: 1, SourceID: hex.EncodeToString(id), CVSRoot: root, Module: module, KeywordMode: "kb", FileMode: "100644", Scope: "all"}
	if err := validateConfig(c); err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return diag.New("io", "init", "create destination directory", err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	r := &Repository{Path: abs, Config: c, Runner: runner}
	if len(entries) == 0 {
		if _, err := runner.Run(ctx, "", nil, nil, "git", "-c", "core.hooksPath=/dev/null", "init", "--bare", "--object-format=sha1", abs); err != nil {
			return err
		}
	}
	if err := r.acquire(); err != nil {
		return err
	}
	defer r.Close()
	if _, err := os.Stat(filepath.Join(abs, "cvsgitrelay.json")); !os.IsNotExist(err) {
		return diag.New("config", "init", "destination already has exporter configuration", err)
	}
	if err := r.validateBare(ctx); err != nil {
		return err
	}
	refs, err := r.allRefs(ctx)
	if err != nil {
		return err
	}
	if len(refs) != 0 {
		return diag.New("conflict", "init", "destination must be an empty dedicated bare repository", nil, "ref_count", len(refs))
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	f, err := os.OpenFile(filepath.Join(abs, "cvsgitrelay.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	r.Manifest = Manifest{Version: 1, ConfigHash: r.configHash(), Refs: map[string]string{}, Members: map[string]map[string]string{}}
	return r.publish(ctx, "", "", "")
}

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
		return fail(diag.New("config", "repository.open", "read exporter configuration", err))
	}
	if info.Mode().Perm()&0077 != 0 {
		return fail(diag.New("config", "repository.open", "cvsgitrelay.json must have mode 0600", nil))
	}
	data, err := os.ReadFile(filepath.Join(abs, "cvsgitrelay.json"))
	if err != nil {
		return fail(err)
	}
	if err := decodeJSON(data, &r.Config); err != nil {
		return fail(diag.New("config", "repository.open", "invalid configuration", err))
	}
	if err := validateConfig(r.Config); err != nil {
		return fail(err)
	}
	runner.Root = r.Config.CVSRoot
	if err := r.validateBare(ctx); err != nil {
		return fail(err)
	}
	refs, err := r.allRefs(ctx)
	if err != nil {
		return fail(err)
	}
	r.StateOID = refs[stateRef]
	if r.StateOID == "" {
		return fail(diag.New("integrity", "repository.open", "missing exporter state ref; restore refs from backup", nil, "ref", stateRef))
	}
	out, err := r.git(ctx, nil, nil, "show", r.StateOID+":manifest.json")
	if err != nil {
		return fail(err)
	}
	if err := decodeJSON(out, &r.Manifest); err != nil {
		return fail(diag.New("integrity", "manifest.read", "invalid manifest", err))
	}
	if r.Manifest.Version != 1 || r.Manifest.ConfigHash != r.configHash() || r.Manifest.Refs == nil || r.Manifest.Members == nil {
		return fail(diag.New("integrity", "manifest.read", "configuration differs from the Git checkpoint or manifest schema is invalid", nil))
	}
	for ref, oid := range r.Manifest.Refs {
		if !ownedDataRef(ref) || !validOID(oid) || refs[ref] != oid {
			return fail(diag.New("conflict", "refs.validate", "managed ref vanished or moved; restore its published value", nil, "ref", ref, "expected", oid, "actual", refs[ref]))
		}
	}
	for ref := range refs {
		if ownedNamespace(ref) && ref != stateRef && r.Manifest.Refs[ref] == "" {
			return fail(diag.New("conflict", "refs.validate", "unexpected ref in exporter namespace", nil, "ref", ref))
		}
	}
	if err := r.validateCheckpoints(ctx); err != nil {
		return fail(err)
	}
	return r, nil
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
		if ownedNamespace(parts[0]) && parts[2] != "" {
			return nil, diag.New("conflict", "refs.enumerate", "symbolic managed refs are forbidden", nil, "ref", parts[0])
		}
		refs[parts[0]] = parts[1]
	}
	return refs, nil
}

func ownedNamespace(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/cvs/") || strings.HasPrefix(ref, "refs/cvsgitrelay/")
}
func ownedDataRef(ref string) bool {
	return ref == trunkRef || strings.HasPrefix(ref, "refs/heads/cvs/branches/") || strings.HasPrefix(ref, "refs/cvsgitrelay/unmapped/")
}

// publish updates both the data ref and its Git manifest atomically. All other
// managed refs are verified in that transaction to detect concurrent edits.
func (r *Repository) publish(ctx context.Context, ref, newOID, oldOID string) error {
	if ref != "" {
		r.Manifest.Refs[ref] = newOID
	}
	data, _ := json.Marshal(r.Manifest)
	blob, err := r.oid(ctx, data, nil, "hash-object", "-w", "--stdin")
	if err != nil {
		return err
	}
	tree, err := r.oid(ctx, []byte("100644 blob "+blob+"\tmanifest.json\x00"), nil, "mktree", "-z")
	if err != nil {
		return err
	}
	args := []string{"commit-tree", tree}
	if r.StateOID != "" {
		args = append(args, "-p", r.StateOID)
	}
	state, err := r.oid(ctx, []byte("cvsgitrelay checkpoint v1\n"), exporterEnv(0, "cvsgitrelay"), args...)
	if err != nil {
		return err
	}
	var commands strings.Builder
	commands.WriteString("start\noption no-deref\n")
	if r.StateOID == "" {
		fmt.Fprintf(&commands, "create %s %s\n", stateRef, state)
	} else {
		fmt.Fprintf(&commands, "update %s %s %s\n", stateRef, state, r.StateOID)
	}
	refs := make([]string, 0, len(r.Manifest.Refs))
	for name := range r.Manifest.Refs {
		refs = append(refs, name)
	}
	slices.Sort(refs)
	for _, name := range refs {
		if name == ref {
			if oldOID == "" {
				fmt.Fprintf(&commands, "create %s %s\n", name, newOID)
			} else {
				fmt.Fprintf(&commands, "update %s %s %s\n", name, newOID, oldOID)
			}
		} else {
			fmt.Fprintf(&commands, "verify %s %s\n", name, r.Manifest.Refs[name])
		}
	}
	commands.WriteString("prepare\ncommit\n")
	if _, err := r.git(ctx, []byte(commands.String()), nil, "update-ref", "--stdin"); err != nil {
		return diag.New("conflict", "publish", "atomic ref publication failed; rerun to reconstruct the checkpoint", err, "ref", ref, "expected_parent", oldOID, "new_commit", newOID, "expected_state", r.StateOID)
	}
	r.StateOID = state
	return nil
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

func exporterEnv(timestamp int64, author string) []string {
	return []string{"GIT_AUTHOR_NAME=" + author, "GIT_AUTHOR_EMAIL=cvs@cvsgitrelay.invalid", "GIT_AUTHOR_DATE=@" + fmt.Sprint(timestamp) + " +0000", "GIT_COMMITTER_NAME=cvsgitrelay", "GIT_COMMITTER_EMAIL=exporter@cvsgitrelay.invalid", "GIT_COMMITTER_DATE=@" + fmt.Sprint(timestamp) + " +0000"}
}
