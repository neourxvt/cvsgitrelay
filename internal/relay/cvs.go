package relay

import (
	"context"
	"errors"
	"strings"
	"time"

	"cvsgitrelay/internal/diag"
)

func (r *Repository) cvs(ctx context.Context, dir string, args ...string) ([]byte, error) {
	prefix := []string{"-f", "-Q", "-d", r.Config.CVSRoot}
	run := r.Runner.Run
	if len(args) > 0 && (args[0] == "rlog" || (len(args) == 2 && args[0] == "checkout" && args[1] == "-c")) {
		run = r.Runner.RunQuiet
	}
	// CVS reads can be repeated after transport failures. Each attempt has
	// fresh output buffers, so partial responses never enter the durable cache.
	retryDelays := [...]time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second}
	for attempt := 0; ; attempt++ {
		out, err := run(ctx, dir, nil, nil, "cvs", append(prefix, args...)...)
		if err == nil || attempt == len(retryDelays) || ctx.Err() != nil || !transientCVSConnection(err) {
			return out, err
		}
		delay := retryDelays[attempt]
		if r.Runner.Log != nil {
			r.Runner.Log.Warn("CVS connection failed; retrying read", "retry", attempt+1, "max_retries", len(retryDelays), "delay", delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, diag.New("timeout", "cvs.retry", "CVS read retry interrupted", ctx.Err())
		case <-timer.C:
		}
	}
}

// Only failed subprocesses with recognizable transport diagnostics qualify.
// Warnings after exit zero, output limits and source errors remain fatal.
func transientCVSConnection(err error) bool {
	var failure *diag.Error
	if !errors.As(err, &failure) || failure.Op != "process.run" || failure.Code != "subprocess" {
		return false
	}
	exitCode, _ := failure.Fields["exit_code"].(int)
	if exitCode <= 0 {
		return false
	}
	stderr, _ := failure.Fields["stderr_tail"].(string)
	stderr = strings.ToLower(stderr)
	for _, diagnostic := range []string{
		"connection reset by peer",
		"connection timed out",
		"connection refused",
		"broken pipe",
		"end of file from server",
		"connection closed by remote host",
	} {
		if strings.Contains(stderr, diagnostic) {
			return true
		}
	}
	return false
}

func (r *Repository) scan(ctx context.Context) (*Inventory, error) {
	layout, definitions, err := r.moduleLayout(ctx)
	if err != nil {
		return nil, err
	}
	policy := r.submodulePolicy()
	selected, _ := selectModules(layout, policy)
	if err := r.validateLayout(layout); err != nil {
		return nil, err
	}
	out, err := r.cvs(ctx, "", "rlog", r.Config.Module)
	r.metadataRequests++
	r.metadataBytes += int64(len(out))
	if err != nil {
		return nil, diag.New(diag.Code(err), "cvs.scan", "full source enumeration failed", err, "module_hex", hexBytes(r.Config.Module))
	}
	dir, _ := sourceDirectory(r.Config.CVSRoot)
	var mappings []rlogMapping
	for _, mapping := range layout {
		mappings = append(mappings, rlogMapping{RCSPrefix: dir + "/" + mapping.Source + "/", SourcePrefix: mapping.Source, DestinationPrefix: mapping.Destination, Excluded: policy == "exclude" && mapping.Destination != ""})
	}
	in, archives, err := parseMappedRlog(out, mappings, r.mirroring)
	if err != nil {
		return nil, err
	}
	if err := r.verifyRlogArchives(ctx, r.Config.Module, archives); err != nil {
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
	in.Layout = selected
	in.Submodules = policy
	if in.NumberingGaps > 0 {
		if r.RequireConsecutiveRevisions {
			return nil, diag.New("source", "cvs.scan", "CVS history skips revision numbers; consecutive revisions are required by configuration", nil,
				"gap_ranges", in.NumberingGaps, "option", "source.require_consecutive_revisions")
		}
		r.Runner.Log.Warn("CVS history skips revision numbers; only listed revisions can be imported", "gap_ranges", in.NumberingGaps)
	}
	r.Runner.Log.Debug("CVS module layout resolved", "module", r.Config.Module, "submodules", policy, "mapping", selected, "excluded_mapping", in.Excluded)
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
