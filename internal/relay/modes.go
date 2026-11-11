package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"cvsgitrelay/internal/diag"
)

// CVS keeps no per-revision permissions. A checkout gives every revision the
// current mode of its RCS archive, and neither rlog nor checkout -p exposes
// that mode, so it is observed through real checkouts into a work directory.

// checkoutObservation checks out module, or one revision of a file, into
// root/tree. A fixed client umask and disabled CVSREAD avoid machine-dependent
// mode changes; -n suppresses module checkout hooks.
func (r *Repository) checkoutObservation(ctx context.Context, root, module, revision string) error {
	args := []string{"-f", "-Q", "-d", r.Config.CVSRoot, "checkout", "-n", "-kb", "-d", "tree"}
	if revision != "" {
		args = append(args, "-r", revision)
	}
	// A trailing slash still expands an identically named composite alias.
	// Explicit relative paths select the physical repository directory or file;
	// included repositories are observed independently through inventory.Layout.
	args = append(args, "./"+module)
	_, err := r.Runner.Run(ctx, root, nil, nil, "sh", append([]string{"-c", `unset CVSREAD; umask 000; exec cvs "$@"`, "cvs"}, args...)...)
	return err
}

// modeProbe records one historical permission probe. The archive mode is a
// property of the RCS file, so an earlier result stays valid while the probed
// revision and the archive location are unchanged.
type modeProbe struct {
	Source    []byte `json:"source"`
	Revision  string `json:"revision"`
	Attic     bool   `json:"attic"`
	Mode      uint32 `json:"mode"`
	Available bool   `json:"available"`
}

// historicalProbeTarget returns the newest live revision, whose checkout
// exposes the archive mode, or nil when no live revision exists.
func historicalProbeTarget(file *File) *Revision {
	var live *Revision
	for _, rev := range file.Revisions {
		if rev.State != "dead" && (live == nil || compareNumber(rev.Number, live.Number) > 0) {
			live = rev
		}
	}
	return live
}

// loadModeProbes reads earlier probe results by source path. The file is only
// an optimization: a missing or unreadable one makes every probe run again.
func (r *Repository) loadModeProbes(path string) map[string]modeProbe {
	cached := map[string]modeProbe{}
	data, err := os.ReadFile(path)
	if err != nil {
		return cached
	}
	var probes []modeProbe
	if decodeJSON(data, &probes) != nil {
		r.Runner.Log.Warn("Ignoring unreadable historical permission cache", "path", path)
		return cached
	}
	for _, probe := range probes {
		if probe.Mode&^0777 == 0 {
			cached[string(probe.Source)] = probe
		}
	}
	return cached
}

// saveModeProbes replaces the cache with exactly the current results, sorted,
// so fresh and incremental observations write identical bytes.
func saveModeProbes(path string, probes []modeProbe) error {
	slices.SortFunc(probes, func(a, b modeProbe) int { return bytes.Compare(a.Source, b.Source) })
	if probes == nil {
		probes = []modeProbe{}
	}
	data, err := json.Marshal(probes)
	if err != nil {
		return err
	}
	return mirrorWrite(path, data)
}

// reusableModeProbe reports whether an earlier probe of file still applies.
func reusableModeProbe(cached map[string]modeProbe, file *File, live *Revision) (modeProbe, bool) {
	probe, ok := cached[file.SourcePath]
	return probe, ok && probe.Revision == live.Number && probe.Attic == file.InAttic
}

// modeObservation configures observeModes.
type modeObservation struct {
	// Roots are the physical directories checked out at their default branch.
	Roots []string
	// RootStage is the progress message logged for each root checkout.
	RootStage string
	// WorkDir receives one temporary directory per checkout. The caller decides
	// when to remove them.
	WorkDir string
	// ProbeCache is the file that keeps historical probe results between runs;
	// Refresh ignores its contents. It is always rewritten with the results.
	ProbeCache string
	Refresh    bool
	// Connections limits concurrent historical probes.
	Connections int
	Progress    *mirrorProgress
	// Visit, when set, sees every directory and regular file of a root
	// checkout (CVS administrative directories excluded) by physical path.
	Visit func(root, physical, path string, info fs.FileInfo) error
}

// observeModes returns the permission bits of every inventory file whose mode
// CVS exposes: those in a root checkout, then those reachable through one
// checkout of their newest live revision. Files with no live revision, or
// whose probe yields no file, are absent from the result.
func (r *Repository) observeModes(ctx context.Context, inventory *Inventory, o modeObservation) (map[string]fs.FileMode, error) {
	modes := map[string]fs.FileMode{}
	for _, source := range o.Roots {
		o.Progress.stage(o.RootStage, "path", source)
		workspace, err := os.MkdirTemp(o.WorkDir, "work-tree-")
		if err != nil {
			return nil, err
		}
		err = r.checkoutObservation(ctx, workspace, source+"/", "")
		if err == nil {
			err = filepath.WalkDir(filepath.Join(workspace, "tree"), func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Name() == "CVS" && entry.IsDir() {
					return filepath.SkipDir
				}
				relative, err := filepath.Rel(filepath.Join(workspace, "tree"), path)
				if err != nil {
					return err
				}
				physical := source
				if relative != "." {
					physical += "/" + filepath.ToSlash(relative)
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					if !info.Mode().IsRegular() {
						return diag.New("unsupported", "mirror.tree", "unsupported checkout file type", nil)
					}
					modes[physical] = info.Mode().Perm()
				}
				if o.Visit != nil {
					return o.Visit(source, physical, path, info)
				}
				return nil
			})
		}
		if err != nil {
			return nil, err
		}
	}
	// Files absent from a default checkout can still expose mode through an older
	// live revision or a branch. No content is fabricated for that metadata probe.
	// The probes are independent checkouts and share the connection limit.
	cached := map[string]modeProbe{}
	if !o.Refresh {
		cached = r.loadModeProbes(o.ProbeCache)
	}
	var probes []*Revision
	var results []modeProbe
	for _, file := range inventory.Files {
		if _, known := modes[file.SourcePath]; known {
			continue
		}
		live := historicalProbeTarget(file)
		if live == nil {
			continue
		}
		if probe, ok := reusableModeProbe(cached, file, live); ok {
			if probe.Available {
				modes[file.SourcePath] = fs.FileMode(probe.Mode)
			}
			results = append(results, probe)
			continue
		}
		probes = append(probes, live)
	}
	slices.SortFunc(probes, func(a, b *Revision) int { return strings.Compare(a.SourcePath, b.SourcePath) })
	r.Runner.Log.Info("Historical CVS file permissions planned", "probes", len(probes), "reused", len(results), "refresh", o.Refresh)
	if len(probes) > 0 {
		// Start this phase once so its clock covers every historical probe.
		o.Progress.stage("Observing historical CVS file permissions", "files", len(probes))
	}
	type observed struct {
		mode  fs.FileMode
		known bool
	}
	fetched := newAllRevisionFetches(ctx, probes, o.Connections, func(ctx context.Context, live *Revision) (observed, error) {
		r.Runner.Log.Debug("Observing historical CVS file permissions", "path", live.SourcePath, "revision", live.Number)
		workspace, err := os.MkdirTemp(o.WorkDir, "work-mode-")
		if err != nil {
			return observed{}, err
		}
		var probe observed
		err = r.checkoutObservation(ctx, workspace, live.SourcePath, live.Number)
		if err == nil {
			err = filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Name() == "CVS" && entry.IsDir() {
					return filepath.SkipDir
				}
				if entry.IsDir() {
					return nil
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				probe = observed{info.Mode().Perm(), true}
				return nil
			})
		}
		return probe, err
	})
	defer fetched.close()
	for _, live := range probes {
		probe, err := fetched.next()
		if err != nil {
			return nil, err
		}
		o.Progress.update("path", live.SourcePath, "revision", live.Number)
		if probe.known {
			modes[live.SourcePath] = probe.mode
		}
		file := inventory.Files[live.SourcePath]
		results = append(results, modeProbe{Source: []byte(live.SourcePath), Revision: live.Number, Attic: file.InAttic, Mode: uint32(probe.mode), Available: probe.known})
	}
	if err := saveModeProbes(o.ProbeCache, results); err != nil {
		return nil, err
	}
	return modes, nil
}
