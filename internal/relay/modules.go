package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"cvsgitrelay/internal/diag"
)

// ModuleMapping separates physical repository directories from their checkout
// destinations. It is pinned in Git with the first published source revision.
type ModuleMapping struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type moduleLeaf struct{ source, checkout string }

// parseModules reads CVS checkout -c output, including indented continuations.
// Options are interpreted only for definitions reachable from the requested
// module; unrelated modules may use features outside our supported subset.
func parseModules(data []byte) (map[string][]string, error) {
	if bytes.ContainsAny(data, "\x00\r") {
		return nil, diag.New("parse", "modules.parse", "unsupported control bytes in module listing", nil)
	}
	definitions := map[string][]string{}
	previous := ""
	for i, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if previous == "" {
				return nil, diag.New("parse", "modules.parse", "module continuation has no definition", nil, "line", i+1)
			}
			definitions[previous] = append(definitions[previous], fields...)
			continue
		}
		if len(fields) < 2 {
			return nil, diag.New("parse", "modules.parse", "module definition is incomplete", nil, "line", i+1)
		}
		if _, exists := definitions[fields[0]]; exists {
			return nil, diag.New("parse", "modules.parse", "duplicate module definition", nil, "module", fields[0])
		}
		previous = fields[0]
		definitions[previous] = fields[1:]
	}
	return definitions, nil
}

func modulePath(path string) error {
	if err := validatePath(path); err != nil {
		return err
	}
	if strings.HasPrefix(path, "-") || strings.ContainsAny(path, "!&*?[]\\\"'") || !utf8.ValidString(path) {
		return diag.New("unsupported", "modules.resolve", "module paths must be literal UTF-8 directory names", nil, "path_hex", hexBytes(path))
	}
	return nil
}

// Resolve only directory aliases and regular directory modules with an optional
// -d destination. File selections, exclusions, hooks and ampersand modules need
// their own proven semantics and are rejected instead of approximated.
func resolveModules(definitions map[string][]string, module string) ([]ModuleMapping, error) {
	visiting := map[string]bool{}
	var visit func(string) ([]moduleLeaf, error)
	visit = func(name string) ([]moduleLeaf, error) {
		if err := modulePath(name); err != nil {
			return nil, err
		}
		if visiting[name] {
			return nil, diag.New("source", "modules.resolve", "cycle in CVS module aliases", nil, "module", name)
		}
		args, defined := definitions[name]
		if !defined {
			return []moduleLeaf{{source: name, checkout: name}}, nil
		}
		visiting[name] = true
		defer delete(visiting, name)
		if len(args) > 0 && args[0] == "-a" {
			if len(args) == 1 {
				return nil, diag.New("source", "modules.resolve", "empty module alias", nil, "module", name)
			}
			var leaves []moduleLeaf
			for _, child := range args[1:] {
				resolved, err := visit(child)
				if err != nil {
					return nil, err
				}
				leaves = append(leaves, resolved...)
			}
			return leaves, nil
		}
		destination := name
		if len(args) >= 2 && args[0] == "-d" {
			destination, args = args[1], args[2:]
		}
		if len(args) != 1 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[0], "&") {
			return nil, diag.New("unsupported", "modules.resolve", "only directory modules, -a aliases and -d destinations are supported", nil, "module", name)
		}
		if err := modulePath(args[0]); err != nil {
			return nil, err
		}
		if err := modulePath(destination); err != nil {
			return nil, err
		}
		return []moduleLeaf{{source: args[0], checkout: destination}}, nil
	}
	leaves, err := visit(module)
	if err != nil {
		return nil, err
	}
	var mappings []ModuleMapping
	for _, leaf := range leaves {
		destination := ""
		if leaf.checkout != module {
			if !strings.HasPrefix(leaf.checkout, module+"/") {
				return nil, diag.New("unsupported", "modules.resolve", "module expands outside its requested checkout root", nil, "module", module, "checkout", leaf.checkout)
			}
			destination = strings.TrimPrefix(leaf.checkout, module+"/")
		}
		mappings = append(mappings, ModuleMapping{Source: leaf.source, Destination: destination})
	}
	slices.SortFunc(mappings, func(a, b ModuleMapping) int { return strings.Compare(a.Source, b.Source) })
	if err := validateMappings(mappings); err != nil {
		return nil, err
	}
	return mappings, nil
}

func validateMappings(mappings []ModuleMapping) error {
	root := false
	for i, mapping := range mappings {
		if err := modulePath(mapping.Source); err != nil {
			return err
		}
		if mapping.Destination == "" {
			root = true
		} else if err := modulePath(mapping.Destination); err != nil {
			return err
		}
		for j := 0; j < i; j++ {
			other := mappings[j]
			if other.Source >= mapping.Source || strings.HasPrefix(mapping.Source, other.Source+"/") || mapping.Destination == other.Destination {
				return diag.New("unsupported", "modules.resolve", "module directories overlap or reuse a checkout destination", nil, "source", mapping.Source, "other_source", other.Source, "destination", mapping.Destination)
			}
		}
	}
	if !root {
		return diag.New("unsupported", "modules.resolve", "module has no directory at its checkout root", nil)
	}
	return nil
}

func sameMappings(a, b []ModuleMapping) bool       { return slices.Equal(a, b) }
func literalMapping(module string) []ModuleMapping { return []ModuleMapping{{Source: module}} }

func (r *Repository) moduleLayout(ctx context.Context) ([]ModuleMapping, map[string][]string, error) {
	out, err := r.cvs(ctx, "", "checkout", "-c")
	if err != nil {
		return nil, nil, diag.New(diag.Code(err), "modules.discover", "cannot read CVS module definitions", err)
	}
	definitions, err := parseModules(out)
	if err != nil {
		return nil, nil, err
	}
	layout, err := resolveModules(definitions, r.Config.Module)
	return layout, definitions, err
}

// Legacy literal mirrors have an implicit identity mapping. A new layout may
// be pinned only before imports, or when making that same mapping explicit.
func (r *Repository) validateLayout(layout []ModuleMapping) error {
	expected := r.Manifest.Layout
	if len(expected) == 0 && len(r.Manifest.Refs) > 0 {
		expected = literalMapping(r.Config.Module)
	}
	if len(expected) > 0 && !sameMappings(expected, layout) {
		return diag.New("integrity", "modules.compare", "CVS module mapping differs from the published source layout", nil, "expected_mapping", expected, "actual_mapping", layout)
	}
	return nil
}

func mappingFingerprint(layout []ModuleMapping) string {
	data, _ := json.Marshal(layout)
	return digest(data)
}
