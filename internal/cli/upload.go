package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/cli/output"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
)

// preparedUpload is the file of a command with CLI.Upload, checked by
// prepareUpload before any host is asked, ready to upload to each host.
type preparedUpload struct {
	op    def.Op // The KindUpload op
	field string // The input field of the command's op set from the upload result
	path  string
	size  int64
	query url.Values // Each sidecar's trimmed content, by upload input field
}

// withoutRequired resolves a copy of s in which field is not required.
func withoutRequired(s *jsonschema.Schema, field string) (*jsonschema.Resolved, error) {
	c := s.CloneSchemas()
	c.Required = slices.DeleteFunc(slices.Clone(c.Required), func(n string) bool { return n == field })
	r, err := c.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve input schema without %q: %w", field, err)
	}
	return r, nil
}

// checkUploadFlags checks that the flags of u, when set, take no reserved
// or input flag and are distinct.
func checkUploadFlags(u *def.CLIUpload, inputs map[string]*inputField) error {
	if u == nil {
		return nil
	}
	flags := []string{u.FileFlag}
	for _, s := range u.Sidecars {
		flags = append(flags, s.Flag)
	}
	seen := map[string]bool{}
	for _, f := range flags {
		_, input := inputs[f]
		if seen[f] || input || slices.Contains(reservedFlags, f) {
			return fmt.Errorf("upload flag --%s is already taken", f)
		}
		seen[f] = true
	}
	return nil
}

// prepareUpload checks the file of op's CLI.Upload and reads its sidecars:
// the file must be a regular file within the upload op's declared ceiling
// (A-11), and each sidecar, the flag's path or else <file><suffix>, a
// regular file within its MaxBytes. Any failure is a usage error, before
// anything is sent. It returns nil when op uploads nothing.
func (a *app) prepareUpload(cmd *cobra.Command, g *opCommand, op def.Op) (*preparedUpload, error) {
	u := op.CLI.Upload
	if u == nil {
		return nil, nil
	}
	upOp := g.uploads[op.ID]
	flags := cmd.Flags()
	path, err := flags.GetString(u.FileFlag)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, usagef("--%s is required: the file to upload", u.FileFlag)
	}
	size, err := checkFile("--"+u.FileFlag+" "+path, path, upOp.Upload.MaxBytes)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for _, s := range u.Sidecars {
		p, err := flags.GetString(s.Flag)
		if err != nil {
			return nil, err
		}
		label := "--" + s.Flag + " " + p
		if !flags.Changed(s.Flag) {
			p = path + s.Suffix
			label = p + " (the default --" + s.Flag + ")"
		} else if p == "" {
			return nil, usagef("--%s must name a file", s.Flag)
		}
		v, err := readSidecar(label, p, int64(s.MaxBytes))
		if err != nil {
			return nil, err
		}
		query.Set(s.Param, v)
	}
	if err := checkUploadInput(upOp, query); err != nil {
		return nil, err
	}
	return &preparedUpload{op: upOp, field: u.Field, path: path, size: size, query: query}, nil
}

// checkFile stats path, which label names in errors: it must be a regular
// file of at most maxBytes. It returns its size.
func checkFile(label, path string, maxBytes int64) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, fileError(label, err)
	}
	return checkInfo(label, st, maxBytes)
}

func checkInfo(label string, st fs.FileInfo, maxBytes int64) (int64, error) {
	switch {
	case !st.Mode().IsRegular():
		return 0, usagef("%s: not a regular file", label)
	case st.Size() > maxBytes:
		return 0, usagef("%s: %d bytes, over the limit of %d bytes", label, st.Size(), maxBytes)
	}
	return st.Size(), nil
}

// fileError is a failure to stat or open a file as a usage error naming it.
func fileError(label string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return usagef("%s: no such file", label)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err // label already names the path
	}
	return usagef("%s: %v", label, err)
}

// readSidecar reads a sidecar file of at most maxBytes, checked as a
// regular file before it is opened, and returns its content trimmed of
// surrounding whitespace.
func readSidecar(label, path string, maxBytes int64) (string, error) {
	// A FIFO or device is refused here, before an open could block on it.
	if _, err := checkFile(label, path, maxBytes); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fileError(label, err)
	}
	defer f.Close() // Read-only: a close error loses nothing
	st, err := f.Stat()
	if err != nil {
		return "", fileError(label, err)
	}
	if _, err := checkInfo(label, st, maxBytes); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", usagef("%s: %v", label, err)
	}
	if int64(len(data)) > maxBytes {
		return "", usagef("%s: over the limit of %d bytes", label, maxBytes)
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", usagef("%s: empty", label)
	}
	return v, nil
}

// checkUploadInput checks the sidecar values against the upload op's input
// schema, as the server will, so a malformed one sends nothing.
func checkUploadInput(op def.Op, query url.Values) error {
	resolved, err := op.InputSchema()
	if err != nil {
		return err
	}
	in := map[string]any{}
	for k := range query {
		in[k] = query.Get(k)
	}
	data, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode upload input: %w", err)
	}
	var inst any
	if err := json.Unmarshal(data, &inst); err != nil {
		return fmt.Errorf("decode upload input: %w", err)
	}
	if err := resolved.Validate(inst); err != nil {
		return usagef("invalid upload input: %v", err)
	}
	return nil
}

// upload stages up on host t through its upload op, then returns op's input
// with up.field set to the upload's result, and its request, carrying the
// headers of req, such as the Idempotency-Key: the upload itself has none.
func (a *app) upload(ctx context.Context, c *client.Client, t hostTarget, op def.Op, up *preparedUpload,
	in map[string]any, req client.Request) (map[string]any, client.Request, error) {
	_, route, err := openapi.Route(up.op)
	if err != nil {
		return nil, client.Request{}, err
	}
	f, err := os.Open(up.path)
	if err != nil {
		return nil, client.Request{}, fileError("--"+op.CLI.Upload.FileFlag+" "+up.path, err)
	}
	defer f.Close() // Read-only: a close error loses nothing
	// The file was checked before any host was asked; a file replaced since
	// would not match the Content-Length.
	st, err := f.Stat()
	if err != nil {
		return nil, client.Request{}, fileError(up.path, err)
	}
	if !st.Mode().IsRegular() || st.Size() != up.size {
		return nil, client.Request{}, fmt.Errorf("%s changed while uploading: it is no longer the %d-byte file checked", up.path, up.size)
	}
	a.uploading(t.Name, up)
	resp, err := c.Upload(ctx, client.UploadRequest{Path: strings.TrimPrefix(route, openapi.Prefix), Query: up.query,
		ContentType: up.op.Upload.ContentTypes[0], Body: f, Size: up.size})
	if err != nil {
		return nil, client.Request{}, err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, client.Request{}, errNotJSON
	}
	var id string
	if err := json.Unmarshal(result[up.field], &id); err != nil || id == "" {
		return nil, client.Request{}, fmt.Errorf("the host answered the upload without %s", up.field)
	}
	hin := maps.Clone(in)
	hin[up.field] = id
	hreq, err := buildRequest(op, hin)
	if err != nil {
		return nil, client.Request{}, err
	}
	hreq.Header = req.Header
	return hin, hreq, nil
}

// uploading reports an upload starting on stderr: a JSON line in a machine
// format, as progress is.
func (a *app) uploading(host string, up *preparedUpload) {
	msg := fmt.Sprintf("uploading %s (%s) to host %s", up.path, sizeText(up.size), hostOr(host))
	if !a.format.Machine() {
		fmt.Fprintf(a.env.Stderr, "%s: %s\n", programName, output.Sanitize(msg, false))
		return
	}
	// A failed write to stderr has nowhere to go.
	_ = output.WriteLine(a.env.Stderr, map[string]any{"level": "info", "host": host, "message": msg,
		"file": up.path, "sizeBytes": up.size})
}

// sizeText is n bytes in MiB to one decimal, or in bytes below 0.1 MiB.
func sizeText(n int64) string {
	const mib = 1 << 20
	if n < mib/10 {
		return strconv.FormatInt(n, 10) + " bytes"
	}
	return strings.TrimSuffix(strconv.FormatFloat(float64(n)/mib, 'f', 1, 64), ".0") + " MiB"
}
