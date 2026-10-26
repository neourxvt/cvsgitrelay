package relay

import (
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

func (m *cvsMirror) observeTree(ctx context.Context, inventory *Inventory) error {
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
	for _, file := range inventory.Files {
		if _, known := m.modes[file.SourcePath]; known {
			continue
		}
		m.modes[file.SourcePath] = 0600
		var live *Revision
		for _, rev := range file.Revisions {
			if rev.State != "dead" && (live == nil || compareNumber(rev.Number, live.Number) > 0) {
				live = rev
			}
		}
		if live == nil {
			continue
		}
		workspace, err := os.MkdirTemp(m.state, "work-mode-")
		if err != nil {
			return err
		}
		err = m.checkoutObservation(ctx, workspace, file.SourcePath, live.Number)
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
				m.modes[file.SourcePath] = info.Mode().Perm()
				m.modeKnown[file.SourcePath] = true
				return nil
			})
		}
		if err != nil {
			return err
		}
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
