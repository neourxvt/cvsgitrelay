package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"cvsgitrelay/internal/diag"
)

// Metadata is the immutable v1 provenance schema. Paths, authors and commit IDs
// use hex so arbitrary source bytes cannot inject commit or machine headers.
type Metadata struct {
	Ref           string            `json:"ref"`
	PathHex       string            `json:"path_hex,omitempty"`
	Revision      string            `json:"revision,omitempty"`
	Predecessor   string            `json:"predecessor,omitempty"`
	State         string            `json:"state,omitempty"`
	Blob          string            `json:"blob,omitempty"`
	AuthorHex     string            `json:"author_hex,omitempty"`
	Timestamp     int64             `json:"timestamp"`
	CommitIDHex   string            `json:"commitid_hex,omitempty"`
	DefaultBranch string            `json:"default_branch,omitempty"`
	LogHash       string            `json:"log_sha256,omitempty"`
	LogEncoding   string            `json:"log_encoding,omitempty"`
	Members       map[string]string `json:"members,omitempty"`
}

type Imported struct {
	OID    string
	Tree   string
	Parent string
	Kind   string
	Meta   Metadata
	Log    []byte
}

func metadata(r *Revision, f *File, blob string) Metadata {
	encoding := "utf8"
	if !utf8.Valid(r.Log) || bytes.IndexByte(r.Log, 0) >= 0 {
		encoding = "base64"
	}
	return Metadata{Ref: r.Ref, PathHex: hexBytes(r.Path), Revision: r.Number, Predecessor: r.Predecessor, State: r.State, Blob: blob, AuthorHex: hexBytes(r.Author), Timestamp: r.Time, CommitIDHex: hexBytes(r.CommitID), DefaultBranch: f.DefaultBranch, LogHash: digest(r.Log), LogEncoding: encoding}
}

func message(source, kind string, m Metadata, log []byte) []byte {
	data, _ := json.Marshal(m)
	body := log
	if m.LogEncoding == "base64" {
		body = []byte(base64.StdEncoding.EncodeToString(log) + "\n")
	}
	header := "cvsgitrelay-Version: 1\ncvsgitrelay-Source-ID: " + source + "\ncvsgitrelay-Kind: " + kind + "\ncvsgitrelay-Data: " + base64.StdEncoding.EncodeToString(data) + "\n\n"
	return append([]byte(header), body...)
}

func (r *Repository) readCommit(ctx context.Context, oid string) (*Imported, error) {
	data, err := r.git(ctx, nil, nil, "cat-file", "commit", oid)
	if err != nil {
		return nil, err
	}
	header, body, ok := bytes.Cut(data, []byte("\n\n"))
	if !ok {
		return nil, diag.New("integrity", "provenance.read", "commit has no message", nil, "commit", oid)
	}
	c := &Imported{OID: oid}
	var author, committer string
	for _, line := range strings.Split(string(header), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			return nil, diag.New("integrity", "provenance.read", "invalid Git commit header", nil, "commit", oid)
		}
		switch name {
		case "tree":
			if c.Tree != "" || !validOID(value) {
				return nil, fmt.Errorf("invalid commit tree")
			}
			c.Tree = value
		case "parent":
			if c.Parent != "" || !validOID(value) {
				return nil, diag.New("integrity", "provenance.read", "import commits must have at most one parent", nil, "commit", oid)
			}
			c.Parent = value
		case "author":
			if author != "" {
				return nil, fmt.Errorf("duplicate author")
			}
			author = value
		case "committer":
			if committer != "" {
				return nil, fmt.Errorf("duplicate committer")
			}
			committer = value
		default:
			return nil, diag.New("integrity", "provenance.read", "unexpected Git commit header", nil, "commit", oid, "field", name)
		}
	}
	h, b, ok := bytes.Cut(body, []byte("\n\n"))
	if !ok {
		return nil, diag.New("integrity", "provenance.read", "missing machine-owned provenance", nil, "commit", oid)
	}
	fields := strings.Split(string(h), "\n")
	if len(fields) != 4 || fields[0] != "cvsgitrelay-Version: 1" || fields[1] != "cvsgitrelay-Source-ID: "+r.Config.SourceID || !strings.HasPrefix(fields[2], "cvsgitrelay-Kind: ") || !strings.HasPrefix(fields[3], "cvsgitrelay-Data: ") {
		return nil, diag.New("integrity", "provenance.read", "unmanaged, corrupt or mismatched-source commit", nil, "commit", oid)
	}
	c.Kind = strings.TrimPrefix(fields[2], "cvsgitrelay-Kind: ")
	encoded := strings.TrimPrefix(fields[3], "cvsgitrelay-Data: ")
	meta, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, diag.New("integrity", "provenance.read", "invalid provenance encoding", err, "commit", oid)
	}
	if err := decodeJSON(meta, &c.Meta); err != nil {
		return nil, diag.New("integrity", "provenance.read", "invalid provenance data", err, "commit", oid)
	}
	c.Log = b
	if c.Meta.LogEncoding == "base64" {
		c.Log, err = base64.StdEncoding.Strict().DecodeString(strings.TrimSuffix(string(b), "\n"))
		if err != nil {
			return nil, err
		}
	}
	if !bytes.Equal(message(r.Config.SourceID, c.Kind, c.Meta, c.Log), body) {
		return nil, diag.New("integrity", "provenance.read", "noncanonical provenance or body", nil, "commit", oid)
	}
	if c.Kind != "revision" && c.Kind != "seed" {
		return nil, diag.New("integrity", "provenance.read", "unknown commit kind", nil, "commit", oid)
	}
	display := "cvsgitrelay"
	if c.Kind == "revision" {
		name, err := hex.DecodeString(c.Meta.AuthorHex)
		if err != nil {
			return nil, err
		}
		display = authorDisplay(string(name))
		if digest(c.Log) != c.Meta.LogHash {
			return nil, diag.New("integrity", "provenance.read", "CVS log digest mismatch", nil, "commit", oid)
		}
	}
	date := strconv.FormatInt(c.Meta.Timestamp, 10) + " +0000"
	if author != display+" <cvs@cvsgitrelay.invalid> "+date || committer != "cvsgitrelay <exporter@cvsgitrelay.invalid> "+date || c.Tree == "" {
		return nil, diag.New("integrity", "provenance.read", "Git identity/time differs from controlled provenance", nil, "commit", oid)
	}
	return c, nil
}

func authorDisplay(author string) string {
	// The reversible original remains in AuthorHex; the Git display name is
	// intentionally ASCII and cannot contain environment/header control bytes.
	return "CVS " + hexBytes(author)
}

func (r *Repository) reconstruct(ctx context.Context) (map[string]*Imported, map[string][]*Imported, error) {
	imports := map[string]*Imported{}
	chains := map[string][]*Imported{}
	for ref, tip := range r.Manifest.Refs {
		seen := map[string]bool{}
		for oid := tip; oid != ""; {
			if seen[oid] {
				return nil, nil, diag.New("integrity", "history.walk", "cycle in commit graph", nil, "ref", ref)
			}
			seen[oid] = true
			c, err := r.readCommit(ctx, oid)
			if err != nil {
				return nil, nil, diag.New("integrity", "history.walk", "cannot recover published history", err, "ref", ref, "commit", oid)
			}
			if c.Meta.Ref != ref {
				return nil, nil, diag.New("integrity", "history.walk", "revision moved to a different ref", nil, "ref", ref, "commit", oid)
			}
			chains[ref] = append(chains[ref], c)
			if c.Kind == "revision" {
				path, err := hex.DecodeString(c.Meta.PathHex)
				if err != nil {
					return nil, nil, err
				}
				k := key(string(path), c.Meta.Revision)
				if previous := imports[k]; previous != nil {
					return nil, nil, diag.New("integrity", "history.walk", "duplicate source revision identity", nil, "path_hex", c.Meta.PathHex, "revision", c.Meta.Revision, "first_commit", previous.OID, "second_commit", oid)
				}
				imports[k] = c
			} else if c.Parent != "" || ref == trunkRef {
				return nil, nil, diag.New("integrity", "history.walk", "branch seed must be an orphan root on a nontrunk ref", nil, "ref", ref, "commit", oid)
			}
			oid = c.Parent
		}
	}
	return imports, chains, nil
}
