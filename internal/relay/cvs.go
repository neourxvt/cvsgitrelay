package relay

import (
	"context"

	"cvsgitrelay/internal/diag"
)

func (r *Repository) cvs(ctx context.Context, dir string, args ...string) ([]byte, error) {
	prefix := []string{"-f", "-Q", "-d", r.Config.CVSRoot}
	if len(args) > 0 && args[0] == "rlog" {
		return r.Runner.RunQuiet(ctx, dir, nil, nil, "cvs", append(prefix, args...)...)
	}
	return r.Runner.Run(ctx, dir, nil, nil, "cvs", append(prefix, args...)...)
}

func (r *Repository) scan(ctx context.Context) (*Inventory, error) {
	out, err := r.cvs(ctx, "", "rlog", r.Config.Module)
	if err != nil {
		return nil, diag.New(diag.Code(err), "cvs.scan", "full source enumeration failed", err, "module_hex", hexBytes(r.Config.Module))
	}
	dir, _ := sourceDirectory(r.Config.CVSRoot)
	in, err := parseRlog(out, dir+"/"+r.Config.Module+"/")
	if err != nil {
		return nil, err
	}
	r.Runner.Log.Debug("source inventory validated", "files", len(in.Files), "revisions", len(in.Revisions), "trunk", in.Trunk, "nontrunk", len(in.Revisions)-in.Trunk, "dead", in.Dead, "vendor", in.Vendor)
	return in, nil
}

func (r *Repository) fetch(ctx context.Context, rev *Revision) ([]byte, error) {
	out, err := r.cvs(ctx, "", "checkout", "-n", "-p", "-kb", "-r", rev.Number, r.Config.Module+"/"+rev.Path)
	if err != nil {
		return nil, diag.New(diag.Code(err), "cvs.fetch", "exact numeric revision retrieval failed", err, "path_hex", hexBytes(rev.Path), "revision", rev.Number, "ref", rev.Ref)
	}
	return out, nil
}
