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

// mirrorModeProbesFile caches historical permission probes in the mirror state.
const mirrorModeProbesFile = "probes.json"

// observeTree checks out every root to observe current modes and directories.
// Files outside those checkouts need one historical checkout each; unless
// refresh is set, results cached for an unchanged probe target are reused, as
// those checkouts dominate the remote requests of an unchanged mirror run.
// Observation checkouts remain in the mirror's work directory until a
// validated mirror is successfully published.
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
		directorySet[source] = true
	}
	modes, _, err := m.source.observeModes(ctx, inventory, modeObservation{
		Roots:       roots,
		RootStage:   "Observing CVS permissions and directories",
		WorkDir:     m.state,
		ProbeCache:  filepath.Join(m.state, mirrorModeProbesFile),
		Refresh:     refresh,
		Connections: connections,
		Progress:    m.progress,
		// Root checkouts also expose directory modes, empty directories and
		// the readable administrative files, which only the mirror keeps.
		Visit: func(root, physical, path string, info fs.FileInfo) error {
			if info.IsDir() {
				m.directoryModes[physical] = info.Mode().Perm()
				directorySet[physical] = true
				return nil
			}
			if root == "CVSROOT" {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				m.administration[physical] = data
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	for path, mode := range modes {
		m.modes[path] = mode
		m.modeKnown[path] = true
	}
	// An archive whose mode CVS does not expose is written with a private mode.
	for _, file := range inventory.Files {
		if _, known := m.modes[file.SourcePath]; !known {
			m.modes[file.SourcePath] = 0600
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
