// Package clientconfig holds the nexus CLI's host configuration: where it
// lives, how it is read and written, and how a command's target host is
// resolved from it, the --host flag and the environment (FR-32, FR-35).
package clientconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/client"
)

// Environment variables that override the configuration for
// non-interactive use.
const (
	EnvHost        = "NEXUS_HOST"        // A configured host name or a URL
	EnvToken       = "NEXUS_TOKEN"       // Replaces the selected host's token
	EnvFingerprint = "NEXUS_FINGERPRINT" // Pins the leaf for a URL in NEXUS_HOST
)

// FileMode is the only mode the configuration is written with. It holds
// tokens, so it is readable by its owner alone.
const FileMode fs.FileMode = 0o600

// dirMode is the mode of the nexus configuration directory when created.
const dirMode fs.FileMode = 0o700

// maxFileBytes bounds how much of the file is read.
const maxFileBytes = 1 << 20

// ReservedHostNames cannot name a host: `--host all` selects every host (9.4).
var ReservedHostNames = []string{"all"}

var hostNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Host is one configured host.
type Host struct {
	URL string `yaml:"url"`
	// Fingerprint pins the leaf certificate's SHA-256. Empty with an https
	// URL means the host was added with system trust.
	Fingerprint string   `yaml:"fingerprint,omitempty"`
	Token       string   `yaml:"token,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
}

// Trust names how a host's certificate is verified.
func (h Host) Trust() string {
	switch {
	case strings.HasPrefix(h.URL, "http://"):
		return "plain-loopback"
	case h.Fingerprint != "":
		return "pin"
	}
	return "system"
}

// Config is the whole configuration file.
type Config struct {
	DefaultHost string          `yaml:"defaultHost,omitempty"`
	Hosts       map[string]Host `yaml:"hosts,omitempty"`
}

// InsecureModeError is a configuration file holding a token that a group or
// other users can read. The CLI refuses to use it and exits 3.
type InsecureModeError struct {
	Path string
	Mode fs.FileMode
}

func (e *InsecureModeError) Error() string {
	return fmt.Sprintf("refusing to read tokens from %s: its mode is %04o, which gives group or others access; run: chmod 600 %s",
		e.Path, e.Mode.Perm(), e.Path)
}

// ErrNoHost means no host was selected and no default is configured.
var ErrNoHost = errors.New("no host selected: pass --host NAME, set " + EnvHost + ", or add a host with `nexus host add`")

// UnknownHostError names a host that is not configured.
type UnknownHostError struct{ Name string }

func (e *UnknownHostError) Error() string {
	return fmt.Sprintf("unknown host %q: see `nexus host get`", e.Name)
}

// Path returns the configuration file path: $XDG_CONFIG_HOME/nexus/config.yaml,
// or $HOME/.config/nexus/config.yaml when XDG_CONFIG_HOME is unset, empty or
// relative (XDG Base Directory Specification 0.8). The XDG path is used on
// macOS too, not ~/Library.
func Path(getenv func(string) string) (string, error) {
	base := getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		home := getenv("HOME")
		if !filepath.IsAbs(home) {
			return "", errors.New("cannot locate the configuration: neither XDG_CONFIG_HOME nor HOME is an absolute path")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "nexus", "config.yaml"), nil
}

// Load reads the configuration at path. A missing file is an empty
// configuration. A file that holds a token and is readable by group or others
// is refused with *InsecureModeError, before any token is used.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxFileBytes)
	}
	c := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.hasToken() && fi.Mode().Perm()&0o077 != 0 {
		return nil, &InsecureModeError{Path: path, Mode: fi.Mode()}
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) hasToken() bool {
	for _, h := range c.Hosts {
		if h.Token != "" {
			return true
		}
	}
	return false
}

func (c *Config) validate() error {
	for name, h := range c.Hosts {
		if err := ValidateHostName(name); err != nil {
			return err
		}
		if _, err := client.ParseURL(h.URL); err != nil {
			return fmt.Errorf("host %q: %w", name, err)
		}
		if h.Fingerprint != "" {
			if _, err := client.NormalizeFingerprint(h.Fingerprint); err != nil {
				return fmt.Errorf("host %q: %w", name, err)
			}
		}
	}
	if c.DefaultHost != "" {
		if _, ok := c.Hosts[c.DefaultHost]; !ok {
			return fmt.Errorf("defaultHost %q is not a configured host", c.DefaultHost)
		}
	}
	return nil
}

// ValidateHostName checks a host name: lower case letters, digits, '.', '_'
// and '-', at most 63 characters, and not a reserved word.
func ValidateHostName(name string) error {
	if !hostNameRE.MatchString(name) {
		return fmt.Errorf("invalid host name %q: use lower-case letters, digits, '.', '_' and '-', starting with a letter or digit, at most 63 characters", name)
	}
	if slices.Contains(ReservedHostNames, name) {
		return fmt.Errorf("host name %q is reserved", name)
	}
	return nil
}

// Save writes c to path with mode 0600, whatever the umask, creating the
// directory with mode 0700 if needed. The write is atomic: a temporary file
// in the same directory is renamed over the old one.
func Save(path string, c *Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(FileMode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	committed = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // Best effort: the rename is already visible
		_ = d.Close()
	}
	return nil
}

// Target is the resolved host a command talks to.
type Target struct {
	Name        string // Configured name, or the URL's host for a URL target
	URL         string
	Fingerprint string // Empty: system trust (https) or plain loopback (http)
	Token       string
}

// Resolve picks the target host. The --host flag (a configured name) wins,
// then NEXUS_HOST (a name or a URL), then the configured default. NEXUS_TOKEN
// replaces the token. A URL in NEXUS_HOST is used without reading the
// configuration, pinned to NEXUS_FINGERPRINT when set and verified by system
// trust otherwise; NEXUS_FINGERPRINT applies to URL targets only. load is
// called only when the configuration is needed.
func Resolve(flagHost string, getenv func(string) string, load func() (*Config, error)) (Target, error) {
	envHost := strings.TrimSpace(getenv(EnvHost))
	var t Target
	switch {
	case flagHost == "" && strings.Contains(envHost, "://"):
		u, err := client.ParseURL(envHost)
		if err != nil {
			return Target{}, fmt.Errorf("%s: %w", EnvHost, err)
		}
		t = Target{Name: u.Hostname(), URL: u.String()}
		if fp := strings.TrimSpace(getenv(EnvFingerprint)); fp != "" {
			if u.Scheme != "https" {
				return Target{}, fmt.Errorf("%s applies to https URLs only", EnvFingerprint)
			}
			norm, err := client.NormalizeFingerprint(fp)
			if err != nil {
				return Target{}, fmt.Errorf("%s: %w", EnvFingerprint, err)
			}
			t.Fingerprint = norm
		}
	default:
		c, err := load()
		if err != nil {
			return Target{}, err
		}
		name := flagHost
		if name == "" {
			name = envHost
		}
		if name == "" {
			name = c.DefaultHost
		}
		if name == "" {
			return Target{}, ErrNoHost
		}
		h, ok := c.Hosts[name]
		if !ok {
			return Target{}, &UnknownHostError{Name: name}
		}
		t = Target{Name: name, URL: h.URL, Fingerprint: h.Fingerprint, Token: h.Token}
	}
	if tok := getenv(EnvToken); tok != "" {
		t.Token = strings.TrimSpace(tok)
	}
	return t, nil
}
