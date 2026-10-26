package relay

import (
	"context"

	"cvsgitrelay/internal/diag"
)

func (r *Repository) cvs(ctx context.Context, dir string, args ...string) ([]byte, error) {
	prefix := []string{"-f", "-Q", "-d", r.Config.CVSRoot}
	if len(args) > 0 && (args[0] == "rlog" || (len(args) == 2 && args[0] == "checkout" && args[1] == "-c")) {
		return r.Runner.RunQuiet(ctx, dir, nil, nil, "cvs", append(prefix, args...)...)
	}
	return r.Runner.Run(ctx, dir, nil, nil, "cvs", append(prefix, args...)...)
}

func (r *Repository) scan(ctx context.Context) (*Inventory, error) {
	layout, definitions, err := r.moduleLayout(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.validateLayout(layout); err != nil {
		return nil, err
	}
	out, err := r.cvs(ctx, "", "rlog", r.Config.Module)
	if err != nil {
		return nil, diag.New(diag.Code(err), "cvs.scan", "full source enumeration failed", err, "module_hex", hexBytes(r.Config.Module))
	}
	dir, _ := sourceDirectory(r.Config.CVSRoot)
	var mappings []rlogMapping
	for _, mapping := range layout {
		mappings = append(mappings, rlogMapping{RCSPrefix: dir + "/" + mapping.Source + "/", SourcePrefix: mapping.Source, DestinationPrefix: mapping.Destination})
	}
	in, err := parseMappedRlog(out, mappings)
	if err != nil {
		return nil, err
	}
	for _, file := range in.Files {
		if _, shadowed := definitions[file.SourcePath]; shadowed {
			return nil, diag.New("unsupported", "cvs.scan", "a module definition shadows an exact repository file path", nil, "source_path", file.SourcePath)
		}
	}
	after, _, err := r.moduleLayout(ctx)
	if err != nil {
		return nil, err
	}
	if !sameMappings(layout, after) {
		return nil, diag.New("source", "modules.compare", "module definitions changed during discovery; retry when source is stable", nil, "before", layout, "after", after)
	}
	in.Layout = layout
	r.Runner.Log.Debug("CVS module layout resolved", "module", r.Config.Module, "mapping", layout)
	r.Runner.Log.Debug("source inventory validated", "files", len(in.Files), "revisions", len(in.Revisions), "trunk", in.Trunk, "nontrunk", len(in.Revisions)-in.Trunk, "dead", in.Dead, "vendor", in.Vendor)
	return in, nil
}

func (r *Repository) fetch(ctx context.Context, rev *Revision) ([]byte, error) {
	// Alias checkout paths can expand to whole modules even with -p. Fetch
	// the validated physical repository file, independently of its Git path.
	path := rev.SourcePath
	if path == "" {
		path = r.Config.Module + "/" + rev.Path
	}
	out, err := r.cvs(ctx, "", "checkout", "-n", "-p", "-kb", "-r", rev.Number, path)
	if err != nil {
		return nil, diag.New(diag.Code(err), "cvs.fetch", "exact numeric revision retrieval failed", err, "path_hex", hexBytes(rev.Path), "revision", rev.Number, "ref", rev.Ref)
	}
	return out, nil
}
