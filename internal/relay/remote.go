package relay

import (
	"context"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
	"time"

	"cvsgitrelay/internal/diag"
)

// The local destination stays the authoritative store: its reftable backend,
// writer lock, publication journal and ref CAS cannot be provided by a hosted
// remote such as GitHub. A configured remote is an extra publication target.
// After the local generation is published, sync pushes the projections and the
// state ref to it, so the remote is a copy that follows the local generation.

// pushTimeout is the least time one push may take. The first push uploads the
// whole history, which can need far longer than an ordinary command.
const pushTimeout = 2 * time.Hour

// pushRefspecs select what the remote receives. All of them are forced,
// because accepted source changes rewrite branches and moved tags. The
// publication journal and local backups are bookkeeping of this destination
// and are never sent; superseded generations are sent, since the remote would
// otherwise lose history that CVS no longer has.
var pushRefspecs = []string{
	"+refs/heads/*:refs/heads/*",
	"+refs/tags/*:refs/tags/*",
	"+refs/cvsgitrelay/*:refs/cvsgitrelay/*",
	"^refs/cvsgitrelay/ops/*",
	"^refs/cvsgitrelay/backup/*",
}

// ValidateRemote rejects a remote that embeds credentials, as a CVSROOT with
// a password is rejected, because the URL ends up in logs and process lists.
// Authentication belongs to SSH keys or an agent.
func ValidateRemote(remote string) error {
	if remote == "" {
		return nil
	}
	if strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " \t\r\n\x00") {
		return diag.New("config", "remote.validate", "the remote must be a Git URL without whitespace and not start with a dash", nil)
	}
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" {
		if _, hasPassword := u.User.Password(); hasPassword {
			return diag.New("config", "remote.validate", "the remote must not contain a password or token; use an SSH key", nil)
		}
	}
	return nil
}

// ValidateRemoteToken checks that a token can be sent to the remote. A token is
// sent only over https, so that it cannot cross the network in clear text, and
// an ssh remote authenticates with keys instead.
func ValidateRemoteToken(remote string) error {
	if u, err := url.Parse(remote); err != nil || u.Scheme != "https" || u.Host == "" {
		return diag.New("config", "remote.validate", "an access token needs an https:// remote", nil)
	}
	return nil
}

// tokenEnv passes the token to Git as an Authorization header scoped to the
// remote URL, so that it is not sent to any other host, for example after a
// redirect. Environment variables keep it out of the command line, which other
// users of the machine can read, and out of the repository configuration.
// GitHub accepts any user name with a token as the password.
func tokenEnv(remote, token string) []string {
	if token == "" {
		return nil
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http." + remote + ".extraHeader", "GIT_CONFIG_VALUE_0=" + header}
}

// pushRemote makes the remote match the local projections. The push is atomic,
// so the remote never shows refs from two generations, and it prunes refs the
// local generation no longer has, such as deleted tags. A remote that is
// already current makes this a cheap no-op, which also repairs a push that an
// earlier sync could not finish. The caller holds the writer lock.
func (r *Repository) pushRemote(ctx context.Context, remote, token string) error {
	runner := *r.Runner
	runner.Timeout = max(runner.Timeout, pushTimeout)
	if token != "" {
		// Diagnostics must not repeat the token or its encoded header, should
		// the server or Git echo them.
		runner.Secrets = append(slices.Clone(runner.Secrets), token, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	}
	// Authentication must never wait for a person: ssh fails instead of
	// prompting, as the sync usually runs from a scheduler.
	env := append([]string{"GIT_SSH_COMMAND=ssh -o BatchMode=yes"}, tokenEnv(remote, token)...)
	args := append([]string{"push", "--atomic", "--prune", "--no-progress", "--porcelain", "--", remote}, pushRefspecs...)
	_, err := runner.Run(ctx, "", env, nil, "git", r.gitArgs(args...)...)
	return err
}

// publishRemote pushes and logs the outcome. The local generation is already
// published, so a failure leaves the remote behind but loses nothing; the
// error is returned after the rest of sync so that a scheduler notices it and
// the next sync pushes again.
func (r *Repository) publishRemote(ctx context.Context, remote, token string) error {
	r.phases.begin("push_remote", "Pushing to the remote")
	start := time.Now()
	if err := r.pushRemote(ctx, remote, token); err != nil {
		return diag.New(diag.Code(err), "remote.push", "the generation is published locally, but pushing it to the remote failed; the next sync pushes again", err)
	}
	if r.Runner.Log != nil {
		r.Runner.Log.Info("Pushed to the remote", "elapsed_ms", time.Since(start).Milliseconds())
	}
	return nil
}
