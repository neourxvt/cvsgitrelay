// Package settings reads the required, user-managed TOML configuration. Git
// checkpoints retain source identity and import state independently of this file.
package settings

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"cvsgitrelay/internal/diag"
)

type Config struct {
	Source      Source      `toml:"source"`
	Destination Destination `toml:"destination"`
	Sync        Sync        `toml:"sync"`
	Verify      Verify      `toml:"verify"`
	Logging     Logging     `toml:"logging"`
	Process     Process     `toml:"process"`
	Debug       Debug       `toml:"debug"`
}

type Source struct {
	CVSRoot    string `toml:"cvsroot"`
	Module     string `toml:"module"`
	Submodules string `toml:"submodules"`
}

type Destination struct {
	Repo string `toml:"repo"`
}
type Sync struct {
	Scope string `toml:"scope"`
}
type Verify struct {
	Mode string `toml:"mode"`
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
	return Config{Source: Source{Submodules: "exclude"}, Sync: Sync{Scope: "all"}, Verify: Verify{Mode: "metadata"},
		Logging: Logging{Format: "text", Level: "info", ErrorStacks: true, ProgressInterval: "15s"},
		Process: Process{CommandTimeout: "5m", MaxOutput: 256 << 20}}
}

func Load(path string) (Config, error) {
	c := defaults()
	if path == "" {
		return c, invalid("config", "a TOML path is required; use cvsgitrelay example.toml")
	}
	if !strings.EqualFold(filepath.Ext(path), ".toml") {
		return c, invalid("config", "configuration filename must end in .toml")
	}
	f, err := os.Open(path)
	if err != nil {
		return c, diag.New("config", "settings.read", "cannot open TOML configuration", err, "file", path)
	}
	defer f.Close()
	// A configuration file should be small; bound allocation before parsing.
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return c, diag.New("config", "settings.read", "cannot read TOML configuration", err, "file", path)
	}
	if len(data) > 1<<20 {
		return c, invalid("config", "TOML configuration exceeds the 1 MiB limit")
	}
	meta, err := toml.Decode(string(data), &c)
	if err != nil {
		// Parser errors can quote input values and surrounding source lines.
		// Keep locations useful without disclosing private connection details.
		fields := []any{"file", path}
		var parse toml.ParseError
		if errors.As(err, &parse) {
			fields = append(fields, "line", parse.Position.Line, "column", parse.Position.Col)
		}
		return c, diag.New("config", "settings.parse", "invalid TOML syntax or value type; check example.toml", nil, fields...)
	}
	if keys := meta.Undecoded(); len(keys) > 0 {
		var names []string
		for _, key := range keys {
			names = append(names, key.String())
		}
		return c, diag.New("config", "settings.parse", "unknown configuration options; check example.toml", nil, "file", path, "keys", names)
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	// Relative destinations belong to the configuration file's directory so
	// scheduled jobs and wrappers do not depend on their working directory.
	if !filepath.IsAbs(c.Destination.Repo) {
		c.Destination.Repo = filepath.Join(filepath.Dir(path), c.Destination.Repo)
	}
	c.Destination.Repo, err = filepath.Abs(c.Destination.Repo)
	if err != nil {
		return c, diag.New("config", "settings.path", "cannot resolve destination path", err)
	}
	return c, nil
}

func (c Config) Validate() error {
	if c.Source.CVSRoot == "" {
		return invalid("source.cvsroot", "CVSROOT is required")
	}
	if c.Source.Module == "" {
		return invalid("source.module", "CVS module is required")
	}
	if c.Destination.Repo == "" {
		return invalid("destination.repo", "bare Git destination is required")
	}
	if c.Source.Submodules != "exclude" && c.Source.Submodules != "include" {
		return invalid("source.submodules", "choose exclude or include")
	}
	if c.Sync.Scope != "all" && c.Sync.Scope != "trunk" {
		return invalid("sync.scope", "choose all or trunk")
	}
	if c.Verify.Mode != "metadata" && c.Verify.Mode != "deep" && c.Verify.Mode != "tip" {
		return invalid("verify.mode", "choose metadata, deep or tip")
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
	if c.Debug.Failpoint != "" {
		stage, ordinal, hasOrdinal := strings.Cut(strings.TrimPrefix(c.Debug.Failpoint, "kill:"), ":")
		valid := false
		for _, allowed := range []string{"scan-start", "scan-complete", "fetch-start", "fetch-complete", "blob-written", "tree-written", "commit-written", "before-publish", "after-publish", "seed-written", "seed-published"} {
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
