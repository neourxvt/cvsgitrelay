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

func mirrorArchivePath(file *File) string {
	path := filepath.FromSlash(file.SourcePath)
	if file.InAttic {
		path = filepath.Join(filepath.Dir(path), "Attic", filepath.Base(path))
	}
	return path + ",v"
}

// A checkout exposes permissions and empty directories absent from rlog. A
// fixed client umask and disabled CVSREAD avoid machine-dependent mode changes;
// -n suppresses module checkout hooks. Observation checkouts remain in the
// mirror's work directory until a validated mirror is successfully published.
func (m *cvsMirror) checkoutObservation(ctx context.Context, root, module, revision string) error {
	args := []string{"-f", "-Q", "-d", m.source.Config.CVSRoot, "checkout", "-n", "-kb", "-d", "tree"}
	if revision != "" {
		args = append(args, "-r", revision)
	}
	// A trailing slash still expands an identically named composite alias.
	// Explicit relative paths select the physical repository directory or file;
	// included repositories are observed independently through inventory.Layout.
	args = append(args, "./"+module)
	_, err := m.source.Runner.Run(ctx, root, nil, nil, "sh", append([]string{"-c", `unset CVSREAD; umask 000; exec cvs "$@"`, "cvs"}, args...)...)
	return err
}

// mirrorModeProbe records one historical permission probe. The archive mode
// is a property of the RCS file, so an earlier result stays valid while the
// probed revision and the archive location are unchanged.
type mirrorModeProbe struct {
	Source    []byte `json:"source"`
	Revision  string `json:"revision"`
	Attic     bool   `json:"attic"`
	Mode      uint32 `json:"mode"`
	Available bool   `json:"available"`
}

const mirrorModeProbesFile = "probes.json"

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
func (m *cvsMirror) loadModeProbes() map[string]mirrorModeProbe {
	cached := map[string]mirrorModeProbe{}
	data, err := os.ReadFile(filepath.Join(m.state, mirrorModeProbesFile))
	if err != nil {
		return cached
	}
	var probes []mirrorModeProbe
	if decodeJSON(data, &probes) != nil {
		m.source.Runner.Log.Warn("Ignoring unreadable historical permission cache", "path", filepath.Join(m.state, mirrorModeProbesFile))
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
// so fresh and incremental mirrors write identical bytes.
func (m *cvsMirror) saveModeProbes(probes []mirrorModeProbe) error {
	slices.SortFunc(probes, func(a, b mirrorModeProbe) int { return bytes.Compare(a.Source, b.Source) })
	if probes == nil {
		probes = []mirrorModeProbe{}
	}
	data, err := json.Marshal(probes)
	if err != nil {
		return err
	}
	return mirrorWrite(filepath.Join(m.state, mirrorModeProbesFile), data)
}

// reusableModeProbe reports whether an earlier probe of file still applies.
func reusableModeProbe(cached map[string]mirrorModeProbe, file *File, live *Revision) (mirrorModeProbe, bool) {
	probe, ok := cached[file.SourcePath]
	return probe, ok && probe.Revision == live.Number && probe.Attic == file.InAttic
}

// observeTree checks out every root to observe current modes and directories.
// Files outside those checkouts need one historical checkout each; unless
// refresh is set, results cached for an unchanged probe target are reused, as
// those checkouts dominate the remote requests of an unchanged mirror run.
func (m *cvsMirror) observeTree(ctx context.Context, inventory *Inventory, connections int, refresh bool) error {
	m.administration = map[string][]byte{}
	directorySet := map[string]bool{"CVSROOT": true}
	roots := []string{"CVSROOT"}
	for _, mapping := range inventory.Layout {
		if !slices.Contains(roots, mapping.Source) {
			roots = append(roots, mapping.Source)
		}
	}
	slices.Sort(roots)
	for _, source := range roots {
		m.progress.stage("Observing CVS permissions and directories", "path", source)
		directorySet[source] = true
		workspace, err := os.MkdirTemp(m.state, "work-tree-")
		if err != nil {
			return err
		}
		err = m.checkoutObservation(ctx, workspace, source+"/", "")
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
				if entry.IsDir() {
					info, err := entry.Info()
					if err != nil {
						return err
					}
					m.directoryModes[physical] = info.Mode().Perm()
					directorySet[physical] = true
					return nil
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return diag.New("unsupported", "mirror.tree", "unsupported checkout file type", nil)
				}
				m.modes[physical] = info.Mode().Perm()
				m.modeKnown[physical] = true
				if source == "CVSROOT" {
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					m.administration[physical] = data
				}
				return nil
			})
		}
		if err != nil {
			return err
		}
	}
	// Files absent from a default checkout can still expose mode through an older
	// live revision or a branch. No content is fabricated for that metadata probe.
	// The probes are independent checkouts and share the connection limit.
	cached := map[string]mirrorModeProbe{}
	if !refresh {
		cached = m.loadModeProbes()
	}
	var probes []*Revision
	var results []mirrorModeProbe
	for _, file := range inventory.Files {
		if _, known := m.modes[file.SourcePath]; known {
			continue
		}
		m.modes[file.SourcePath] = 0600
		live := historicalProbeTarget(file)
		if live == nil {
			continue
		}
		if probe, ok := reusableModeProbe(cached, file, live); ok {
			if probe.Available {
				m.modes[file.SourcePath] = fs.FileMode(probe.Mode)
				m.modeKnown[file.SourcePath] = true
			}
			results = append(results, probe)
			continue
		}
		probes = append(probes, live)
	}
	slices.SortFunc(probes, func(a, b *Revision) int { return strings.Compare(a.SourcePath, b.SourcePath) })
	m.source.Runner.Log.Info("Historical CVS file permissions planned", "probes", len(probes), "reused", len(results), "refresh", refresh)
	if len(probes) > 0 {
		// Start this phase once so its clock covers every historical probe.
		m.progress.stage("Observing historical CVS file permissions", "files", len(probes))
	}
	type modeProbe struct {
		mode  fs.FileMode
		known bool
	}
	fetched := newAllRevisionFetches(ctx, probes, connections, func(ctx context.Context, live *Revision) (modeProbe, error) {
		m.source.Runner.Log.Debug("Observing historical CVS file permissions", "path", live.SourcePath, "revision", live.Number)
		workspace, err := os.MkdirTemp(m.state, "work-mode-")
		if err != nil {
			return modeProbe{}, err
		}
		var probe modeProbe
		err = m.checkoutObservation(ctx, workspace, live.SourcePath, live.Number)
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
				probe = modeProbe{info.Mode().Perm(), true}
				return nil
			})
		}
		return probe, err
	})
	defer fetched.close()
	for _, live := range probes {
		probe, err := fetched.next()
		if err != nil {
			return err
		}
		m.progress.update("path", live.SourcePath, "revision", live.Number)
		if probe.known {
			m.modes[live.SourcePath] = probe.mode
			m.modeKnown[live.SourcePath] = true
		}
		file := inventory.Files[live.SourcePath]
		results = append(results, mirrorModeProbe{Source: []byte(live.SourcePath), Revision: live.Number, Attic: file.InAttic, Mode: uint32(probe.mode), Available: probe.known})
	}
	if err := m.saveModeProbes(results); err != nil {
		return err
	}
	m.directories = nil
	for directory := range directorySet {
		m.directories = append(m.directories, directory)
	}
	slices.Sort(m.directories)
	return nil
}

// Preserve readable administration as data. Active local configuration remains
// fixed, so source hook scripts and server paths are never enabled implicitly.
func (m *cvsMirror) preserveAdministration(staging string, inventory *Inventory) error {
	// Preserve observed tree metadata explicitly, including when CVS exposes
	// no permissions because an archive has never had a live revision.
	type observation struct {
		Path      []byte
		Directory bool
		Mode      uint32
		Available bool
	}
	var observed []observation
	for _, directory := range m.directories {
		mode, known := m.directoryModes[directory]
		observed = append(observed, observation{[]byte(directory), true, uint32(mode), known})
	}
	var sourcePaths []string
	for path := range inventory.Files {
		sourcePaths = append(sourcePaths, path)
	}
	slices.Sort(sourcePaths)
	for _, path := range sourcePaths {
		observed = append(observed, observation{[]byte(path), false, uint32(m.modes[path]), m.modeKnown[path]})
	}
	data, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(staging, mirrorStateDir), 0700); err != nil {
		return err
	}
	if err = mirrorWrite(filepath.Join(staging, mirrorStateDir, "tree.json"), data); err != nil {
		return err
	}
	var names []string
	for name := range m.administration {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !strings.HasPrefix(name, "CVSROOT/") {
			return diag.New("internal", "mirror.admin", "invalid administrative path", nil)
		}
		path := filepath.Join(staging, mirrorStateDir, "administration", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := mirrorWrite(path, m.administration[name]); err != nil {
			return err
		}
	}
	return nil
}
