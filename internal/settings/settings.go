// Package settings reads the required, user-managed TOML configuration. Git
// generations retain source identity and import state independently of this file.
package settings

import (
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/relay"
	"cvsgitrelay/internal/secret"
)

type Config struct {
	// Name identifies a repository of a file with [[repository]] entries and
	// is empty for a file that describes a single repository.
	Name        string      `toml:"-"`
	Source      Source      `toml:"source"`
	Destination Destination `toml:"destination"`
	Sync        Sync        `toml:"sync"`
	Verify      Verify      `toml:"verify"`
	Logging     Logging     `toml:"logging"`
	Process     Process     `toml:"process"`
	Maintenance Maintenance `toml:"maintenance"`
	Debug       Debug       `toml:"debug"`
	// Authors maps CVS usernames to "Name <email>" Git identities. It is fixed
	// when a destination is initialized.
	Authors map[string]string `toml:"authors"`
	// Branches selects and renames the CVS branches that become Git branches.
	// It is fixed when a destination is initialized.
	Branches map[string]string `toml:"branches"`
}

type Source struct {
	SourceID                    string `toml:"source_id"`
	CVSRoot                     string `toml:"cvsroot"`
	Module                      string `toml:"module"`
	Submodules                  string `toml:"submodules"`
	RequireConsecutiveRevisions bool   `toml:"require_consecutive_revisions"`
}

type Destination struct {
	Repo string `toml:"repo"`
	// Remote is an optional Git URL, such as a GitHub repository, that sync
	// pushes every published generation to. Repo stays the working store.
	Remote string `toml:"remote"`
	// RemoteSecret is the access token for an https Remote, encoded by
	// "cvsgitrelay encode-secret", or the path of a file that holds it.
	RemoteSecret string `toml:"remote_secret"`
	// History is the history model, fixed when the destination is created:
	// "growing" only ever adds commits, so the published refs only move
	// forward, and "canonical" rebuilds history from the source alone and
	// rewrites it when CVS history changes. Empty keeps the model of an
	// existing destination and gives a new one DefaultHistory.
	History string `toml:"history"`
}

// DefaultHistory is the history model of a new destination whose
// configuration does not name one.
const DefaultHistory = "growing"

// RemoteToken returns the decoded access token for the remote, or "" when no
// remote or secret is configured.
func (c Config) RemoteToken() (string, error) {
	if c.Destination.Remote == "" || c.Destination.RemoteSecret == "" {
		return "", nil
	}
	return secret.Load(c.Destination.RemoteSecret)
}

type Sync struct {
	Scope       string `toml:"scope"`
	Connections int    `toml:"connections"`
	// AcceptHistoryChanges publishes a source whose accepted revisions
	// changed or disappeared, keeping the replaced generation, instead of
	// stopping until a person intervenes.
	AcceptHistoryChanges bool `toml:"accept_history_changes"`
}
type Verify struct {
	// Mode is fixed when a destination is created. Empty keeps the mode of
	// an existing destination and gives a new one DefaultVerifyMode, so a
	// configuration without it works for destinations of either mode.
	Mode string `toml:"mode"`
}

// DefaultVerifyMode is the verification policy of a new destination whose
// configuration does not name one. Old contents are taken from the cache;
// the scheduled content check reads them again.
const DefaultVerifyMode = "trust-append-only"

// Maintenance schedules Git GC of the destination as part of sync.
type Maintenance struct {
	// IntervalDays is the least number of days between two GC runs; 0
	// disables them.
	IntervalDays int `toml:"interval_days"`
	// VerifyDays is the least number of days between two syncs that read
	// every revision from the source instead of the trust-append-only
	// content cache; 0 disables them.
	VerifyDays int `toml:"verify_days"`
}

type Debug struct {
	Failpoint string `toml:"failpoint"`
}

type Logging struct {
	Format           string `toml:"format"`
	Level            string `toml:"level"`
	ErrorStacks      bool   `toml:"error_stacks"`
	ProgressInterval string `toml:"progress_interval"`
}

type Process struct {
	CommandTimeout string `toml:"command_timeout"`
	MaxOutput      int    `toml:"max_output"`
}

func defaults() Config {
	return Config{Source: Source{Submodules: "exclude"}, Sync: Sync{Scope: "all", Connections: 4, AcceptHistoryChanges: true}, Maintenance: Maintenance{IntervalDays: 30, VerifyDays: 30},
		Logging: Logging{Format: "text", Level: "info", ErrorStacks: true, ProgressInterval: "15s"},
		Process: Process{CommandTimeout: "5m", MaxOutput: 256 << 20}}
}

// Load reads a configuration file that describes exactly one repository. Use
// LoadAll for files that may hold several.
func Load(path string) (Config, error) {
	all, err := LoadAll(path)
	if err != nil {
		return defaults(), err
	}
	if len(all) != 1 {
		return defaults(), invalid("repository", "the configuration holds several repositories; use LoadAll")
	}
	return all[0], nil
}

// document is the TOML file: the top-level tables, which are the defaults of
// every repository, and the optional [[repository]] entries.
type document struct {
	Config
	Repositories []toml.Primitive `toml:"repository"`
}

// entry is one [[repository]]. It holds the tables that may differ between
// repositories; logging, process and debug settings belong to the whole run.
type entry struct {
	Name        string            `toml:"name"`
	Source      Source            `toml:"source"`
	Destination Destination       `toml:"destination"`
	Sync        Sync              `toml:"sync"`
	Verify      Verify            `toml:"verify"`
	Maintenance Maintenance       `toml:"maintenance"`
	Authors     map[string]string `toml:"authors"`
	Branches    map[string]string `toml:"branches"`
}

// namePattern limits repository names to what is safe in logs, file names and
// the --only flag.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LoadAll reads the configuration and returns one Config for every repository
// to sync, in file order.
//
// Without [[repository]] entries the file describes a single repository with
// the top-level tables, as it always did, and its Name is empty. With entries,
// the top-level tables are defaults: an entry repeats a table to override the
// keys it names, and the authors and branches maps are merged key by key.
// "{name}" in source_id, module, destination.repo, destination.remote and
// destination.remote_secret stands for the entry's name, so that the entries
// of similar repositories need nothing but a name.
func LoadAll(path string) ([]Config, error) {
	doc := document{Config: defaults()}
	if path == "" {
		return nil, invalid("config", "a TOML path is required; use cvsgitrelay example.toml")
	}
	if !strings.EqualFold(filepath.Ext(path), ".toml") {
		return nil, invalid("config", "configuration filename must end in .toml")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, diag.New("config", "settings.read", "cannot open TOML configuration", err, "file", path)
	}
	defer f.Close()
	// A configuration file should be small; bound allocation before parsing.
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, diag.New("config", "settings.read", "cannot read TOML configuration", err, "file", path)
	}
	if len(data) > 1<<20 {
		return nil, invalid("config", "TOML configuration exceeds the 1 MiB limit")
	}
	meta, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, parseError(err, path)
	}
	dir := filepath.Dir(path)
	var configs []Config
	if len(doc.Repositories) == 0 {
		configs = []Config{doc.Config}
	}
	seen := map[string]bool{}
	for _, primitive := range doc.Repositories {
		// Start from the defaults; keys the entry names replace them.
		e := entry{Source: doc.Source, Destination: doc.Destination, Sync: doc.Sync, Verify: doc.Verify, Maintenance: doc.Maintenance,
			Authors: maps.Clone(doc.Authors), Branches: maps.Clone(doc.Branches)}
		if err := meta.PrimitiveDecode(primitive, &e); err != nil {
			return nil, parseError(err, path)
		}
		if !namePattern.MatchString(e.Name) {
			return nil, invalid("repository.name", "each [[repository]] needs a name of 1..64 letters, digits, dots, dashes and underscores")
		}
		if seen[e.Name] {
			return nil, diag.New("config", "settings.validate", "repository names must be unique", nil, "option", "repository.name", "name", e.Name)
		}
		seen[e.Name] = true
		c := doc.Config
		c.Name, c.Source, c.Destination, c.Sync, c.Verify, c.Maintenance, c.Authors, c.Branches = e.Name, e.Source, e.Destination, e.Sync, e.Verify, e.Maintenance, e.Authors, e.Branches
		expandName(&c)
		configs = append(configs, c)
	}
	// All keys of the file and of its entries are known by now.
	if keys := meta.Undecoded(); len(keys) > 0 {
		var names []string
		for _, key := range keys {
			names = append(names, key.String())
		}
		return nil, diag.New("config", "settings.parse", "unknown configuration options; check example.toml", nil, "file", path, "keys", names)
	}
	for i := range configs {
		c := &configs[i]
		// Relative files and destinations belong to the configuration file's
		// directory so scheduled jobs and wrappers do not depend on their
		// working directory. The secret is read while validating.
		if v := c.Destination.RemoteSecret; v != "" && !strings.HasPrefix(v, secret.Prefix) && !filepath.IsAbs(v) {
			c.Destination.RemoteSecret = filepath.Join(dir, v)
		}
		if err := c.Validate(); err != nil {
			if c.Name != "" {
				err = diag.New("config", "settings.validate", "invalid configuration of repository "+c.Name, err, "repository", c.Name)
			}
			return nil, err
		}
		if !filepath.IsAbs(c.Destination.Repo) {
			c.Destination.Repo = filepath.Join(dir, c.Destination.Repo)
		}
		if c.Destination.Repo, err = filepath.Abs(c.Destination.Repo); err != nil {
			return nil, diag.New("config", "settings.path", "cannot resolve destination path", err)
		}
	}
	return configs, checkDistinct(configs)
}

// parseError reports a syntax or type error. Parser errors can quote input
// values and surrounding source lines, so keep locations useful without
// disclosing private connection details.
func parseError(err error, path string) error {
	fields := []any{"file", path}
	var parse toml.ParseError
	if errors.As(err, &parse) {
		fields = append(fields, "line", parse.Position.Line, "column", parse.Position.Col)
	}
	return diag.New("config", "settings.parse", "invalid TOML syntax or value type; check example.toml", nil, fields...)
}

// expandName replaces "{name}" with the repository name where a value can
// differ per repository.
func expandName(c *Config) {
	for _, field := range []*string{&c.Source.SourceID, &c.Source.Module, &c.Destination.Repo, &c.Destination.Remote, &c.Destination.RemoteSecret} {
		*field = strings.ReplaceAll(*field, "{name}", c.Name)
	}
}

// checkDistinct rejects repositories that would write to the same place. Two
// syncs into one destination would fight over its lock, and two forced pushes
// into one remote would overwrite each other.
func checkDistinct(configs []Config) error {
	repos, remotes := map[string]string{}, map[string]string{}
	for _, c := range configs {
		if other, ok := repos[c.Destination.Repo]; ok {
			return diag.New("config", "settings.validate", "two repositories share one destination.repo", nil, "option", "destination.repo", "repositories", []string{other, c.Name})
		}
		repos[c.Destination.Repo] = c.Name
		if c.Destination.Remote == "" {
			continue
		}
		if other, ok := remotes[c.Destination.Remote]; ok {
			return diag.New("config", "settings.validate", "two repositories share one destination.remote", nil, "option", "destination.remote", "repositories", []string{other, c.Name})
		}
		remotes[c.Destination.Remote] = c.Name
	}
	return nil
}

func (c Config) Validate() error {
	if c.Source.SourceID == "" {
		return invalid("source.source_id", "an explicit stable logical source ID is required")
	}
	if c.Source.CVSRoot == "" {
		return invalid("source.cvsroot", "CVSROOT is required")
	}
	if c.Source.Module == "" {
		return invalid("source.module", "CVS module is required")
	}
	if c.Destination.Repo == "" {
		return invalid("destination.repo", "bare Git destination is required")
	}
	if err := relay.ValidateRemote(c.Destination.Remote); err != nil {
		return invalid("destination.remote", "use a Git URL without whitespace, a leading dash, or an embedded password or token")
	}
	if c.Destination.RemoteSecret != "" {
		if _, err := secret.Load(c.Destination.RemoteSecret); err != nil {
			return invalid("destination.remote_secret", err.Error())
		}
		// Without a remote the secret is unused, so that --remote "" can turn pushing off.
		if c.Destination.Remote != "" {
			if err := relay.ValidateRemoteToken(c.Destination.Remote); err != nil {
				return invalid("destination.remote_secret", "an access token can only be used with an https:// destination.remote")
			}
		}
	}
	if c.Destination.History != "" && c.Destination.History != "growing" && c.Destination.History != "canonical" {
		return invalid("destination.history", "choose growing or canonical")
	}
	if c.Source.Submodules != "exclude" && c.Source.Submodules != "include" {
		return invalid("source.submodules", "choose exclude or include")
	}
	if c.Sync.Scope != "all" {
		return invalid("sync.scope", "v2 requires all; partial scopes cannot publish")
	}
	if c.Sync.Connections < 1 {
		return invalid("sync.connections", "CVS connection limit must be positive")
	}
	if c.Verify.Mode != "" && c.Verify.Mode != "full" && c.Verify.Mode != "trust-append-only" {
		return invalid("verify.mode", "choose full or trust-append-only")
	}
	if c.Logging.Format != "text" && c.Logging.Format != "json" {
		return invalid("logging.format", "choose text or json")
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.Logging.Level)); err != nil {
		return invalid("logging.level", "choose debug, info, warn or error")
	}
	if duration, err := time.ParseDuration(c.Logging.ProgressInterval); err != nil || duration < 0 {
		return invalid("logging.progress_interval", "use a nonnegative Go duration, such as 15s or 0s")
	}
	if duration, err := time.ParseDuration(c.Process.CommandTimeout); err != nil || duration <= 0 {
		return invalid("process.command_timeout", "use a positive Go duration, such as 5m")
	}
	if c.Process.MaxOutput <= 0 {
		return invalid("process.max_output", "output byte limit must be positive")
	}
	if c.Maintenance.IntervalDays < 0 {
		return invalid("maintenance.interval_days", "use a number of days, or 0 to disable maintenance")
	}
	if c.Maintenance.VerifyDays < 0 {
		return invalid("maintenance.verify_days", "use a number of days, or 0 to disable content checks")
	}
	for name, identity := range c.Authors {
		if name == "" || len(name) > canonical.MaxAuthor {
			return invalid("authors", "CVS usernames must contain 1..256 bytes")
		}
		if !canonical.ValidIdentity(identity) {
			return invalid("authors", "each identity must have the form \"Name <email>\" without angle brackets or line breaks inside")
		}
	}
	if err := canonical.ValidateBranchMap(c.Branches); err != nil {
		return invalid("branches", err.Error())
	}
	if c.Debug.Failpoint != "" {
		stage, ordinal, hasOrdinal := strings.Cut(strings.TrimPrefix(c.Debug.Failpoint, "kill:"), ":")
		valid := false
		for _, allowed := range []string{"scan-start", "scan-complete", "fetch-complete", "blob-written", "tree-written", "commit-written", "before-publish", "after-publish", "objects-written", "before-journal", "journal-written", "projection-updated", "before-journal-cleanup"} {
			if stage == allowed {
				valid = true
				break
			}
		}
		if !valid {
			return invalid("debug.failpoint", "unknown interruption stage; see example.toml")
		}
		if hasOrdinal {
			n, err := strconv.Atoi(ordinal)
			if err != nil || n < 1 {
				return invalid("debug.failpoint", "interruption ordinal must be positive")
			}
		}
	}
	return nil
}

func invalid(option, message string) error {
	return diag.New("config", "settings.validate", message, nil, "option", option)
}
