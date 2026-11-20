// Package config loads and validates the single nexusd configuration file.
//
// Every key is checked against the Go types here at every depth, and every
// problem is reported with its file, line and column, so validate-config can
// print them all at once.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	servicesspec "github.com/mauktik/nexus/internal/providers/services/spec"
)

// DefaultPath is where the installer writes the configuration.
const DefaultPath = "/etc/nexus/nexusd.yaml"

// maxFileBytes bounds how much of the file is read. A real config is a few KB.
const maxFileBytes = 4 << 20

// TLS modes.
const (
	TLSAuto  = "auto"
	TLSFiles = "files"
	TLSOff   = "off"
)

// Config is the whole configuration file.
type Config struct {
	Host      Host      `yaml:"host"`
	Listen    []string  `yaml:"listen"`
	TLS       TLS       `yaml:"tls"`
	Limits    Limits    `yaml:"limits"`
	Auth      Auth      `yaml:"auth"`
	Audit     Audit     `yaml:"audit"`
	MCP       MCP       `yaml:"mcp"`
	Providers Providers `yaml:"providers"`

	file string
	locs locations
}

type Host struct {
	Name string `yaml:"name"` // Empty means the OS hostname
}

type TLS struct {
	Mode      string   `yaml:"mode"`
	CertFile  string   `yaml:"certFile"`
	KeyFile   string   `yaml:"keyFile"`
	ExtraSANs []string `yaml:"extraSANs"`
}

type Limits struct {
	MaxBodyBytes       int64 `yaml:"maxBodyBytes"`
	StorageBudgetBytes int64 `yaml:"storageBudgetBytes"`
	OutputCapBytes     int64 `yaml:"outputCapBytes"`
}

type Auth struct {
	FailureLimitPerMinute int `yaml:"failureLimitPerMinute"`
}

// MaxAuditRetentionDays bounds audit.retentionDays. The retention period is a
// time.Duration, which wraps negative past about 106,751 days.
const MaxAuditRetentionDays = 36500

type Audit struct {
	RetentionDays int `yaml:"retentionDays"`
}

type MCP struct {
	AllowedOrigins []string `yaml:"allowedOrigins"`
}

// Providers holds one section per provider. Each section keeps its YAML node
// so the provider can decode it strictly into its own type through
// Section.Decode, which is the closure handed to Provider.Configure.
type Providers struct {
	Docker   Section[DockerConfig]       `yaml:"docker"`
	Apt      Section[AptConfig]          `yaml:"apt"`
	Scripts  Section[ScriptsConfig]      `yaml:"scripts"`
	Nginx    Section[NginxConfig]        `yaml:"nginx"`
	Nexusd   Section[NexusdConfig]       `yaml:"nexusd"`
	Pihole   Section[PiholeConfig]       `yaml:"pihole"`
	Host     Section[HostProviderConfig] `yaml:"host"`
	Services Section[ServicesConfig]     `yaml:"services"`
}

// DockerConfig is the docker provider section.
type DockerConfig struct {
	Enabled             bool          `yaml:"enabled"`
	Socket              string        `yaml:"socket"`
	ConfigDir           string        `yaml:"configDir"`
	UpdateHealthTimeout time.Duration `yaml:"updateHealthTimeout"`
	UpdateMaxRuntime    time.Duration `yaml:"updateMaxRuntime"`
	Compose             ComposeConfig `yaml:"compose"`
}

// ComposeConfig is how the docker provider judges a Compose project's files.
type ComposeConfig struct {
	// TrustedOwners names users, besides root, who may own the project's
	// files and the directories above them (SEC-1). Each must exist on the
	// host; root is always trusted, so listing it changes nothing.
	TrustedOwners []string `yaml:"trustedOwners"`
}

// AptConfig is the apt provider section.
type AptConfig struct {
	Enabled    bool          `yaml:"enabled"`
	LockWait   time.Duration `yaml:"lockWait"`
	MaxRuntime time.Duration `yaml:"maxRuntime"`
}

// ScriptsConfig is the scripts provider section, including the catalog.
type ScriptsConfig struct {
	Enabled bool     `yaml:"enabled"`
	Catalog []Script `yaml:"catalog"`
}

// NginxConfig is the nginx provider section. An empty IncludeDir means the
// distribution's default, which the provider resolves; it is never written
// back here.
type NginxConfig struct {
	Enabled             bool          `yaml:"enabled"`
	IncludeDir          string        `yaml:"includeDir"`
	IncludeSuffix       string        `yaml:"includeSuffix"`
	DisabledSuffix      string        `yaml:"disabledSuffix"`
	Service             string        `yaml:"service"`
	TestTimeout         time.Duration `yaml:"testTimeout"`
	StateTimeout        time.Duration `yaml:"stateTimeout"`
	ReloadVerifyTimeout time.Duration `yaml:"reloadVerifyTimeout"`
	MaxConfigBytes      int64         `yaml:"maxConfigBytes"`
}

// Bounds of the nexusd provider section. The upload ceiling is
// nexusdspec.MaxUploadBytesCeiling, the cap the upload route is declared with.
const (
	MinNexusdHealthTimeout  = 5 * time.Second
	MaxNexusdHealthTimeout  = 10 * time.Minute
	MinNexusdMaxUploadBytes = 1 << 20
)

// NexusdConfig is the nexusd provider section: managing the daemon itself.
type NexusdConfig struct {
	Enabled bool `yaml:"enabled"`
	// HealthTimeout bounds how long a restarted, updated or rolled-back
	// nexusd may take to pass the health check.
	HealthTimeout time.Duration      `yaml:"healthTimeout"`
	Update        NexusdUpdateConfig `yaml:"update"`
}

// NexusdUpdateConfig is how nexusd accepts a release to update to.
type NexusdUpdateConfig struct {
	// MaxUploadBytes caps one uploaded release tarball.
	MaxUploadBytes int64 `yaml:"maxUploadBytes"`
	// TrustedKeys are the Ed25519 public keys a release signature must
	// verify under, each the standard base64 of the raw 32 bytes that
	// release.ParsePublicKey takes. None means updates are not configured.
	// They change only by editing the file.
	TrustedKeys []string `yaml:"trustedKeys"`
}

// PiholeConfig is the pihole provider section.
type PiholeConfig struct {
	Enabled bool   `yaml:"enabled"`
	DNSFile string `yaml:"dnsFile"` // The dnsmasq file Nexus owns, directly in PiholeDNSDir
}

// HostProviderConfig is the host provider section: a read-only overview of
// the host, with a sampler that runs only while the provider is enabled.
type HostProviderConfig struct {
	Enabled bool `yaml:"enabled"`
	// SampleInterval is how often the sampler reads the host's counters.
	SampleInterval time.Duration `yaml:"sampleInterval"`
	// HistoryDuration is how far back the in-memory metrics history goes.
	HistoryDuration time.Duration `yaml:"historyDuration"`
	// SourceTimeout bounds one read of a single data source, such as a
	// statfs call or a command.
	SourceTimeout time.Duration `yaml:"sourceTimeout"`
}

// ServicesConfig is the services provider section: system services, the
// allow-list of those start, stop and restart may act on, and the guarded
// host reboot (services-provider FR-1, FR-8, FR-10). The allow-list and
// reboot.enabled change only by editing the file: config set refuses lists
// and reboot.enabled=true.
type ServicesConfig struct {
	Enabled bool             `yaml:"enabled"`
	Control []ServiceControl `yaml:"control"`
	Reboot  ServicesReboot   `yaml:"reboot"`
}

// ServiceControl is one allow-list entry. An absent actions grants all
// three, and an absent timeout is servicesspec.DefaultControlTimeout.
type ServiceControl struct {
	Name    string        `yaml:"name"`    // Without .service
	Actions []string      `yaml:"actions"` // Subset of start, stop and restart
	Timeout time.Duration `yaml:"timeout"` // How long the service may take to reach the target state
}

// ServicesReboot is the guarded host reboot.
type ServicesReboot struct {
	Enabled bool `yaml:"enabled"`
	// Timeout is how long after the trigger the host must go down before
	// the reboot fails reboot-did-not-happen.
	Timeout time.Duration `yaml:"timeout"`
}

// applyDefaults fills each allow-list entry's absent keys, which the strict
// decoder cannot pre-fill because it builds list entries from zero. A key is
// absent when the decoder recorded no location for it, so an explicit
// timeout: 0s stays zero and is refused by validate-config.
func (c *ServicesConfig) applyDefaults(locs locations, path string) {
	for i := range c.Control {
		p := fmt.Sprintf("%s.control[%d]", path, i)
		if _, ok := locs[p+".actions"]; !ok {
			c.Control[i].Actions = append([]string(nil), servicesspec.Actions...)
		} else if c.Control[i].Actions == nil {
			// `actions:` or `actions: ~`: the key is there with no list, which
			// grants none, as `actions: []` does. Only an absent key means all
			// three.
			c.Control[i].Actions = []string{}
		}
		if _, ok := locs[p+".timeout"]; !ok {
			c.Control[i].Timeout = servicesspec.DefaultControlTimeout
		}
	}
}

// defaulter is a section type with defaults inside lists: Parse and
// Section.Decode both apply them after decoding.
type defaulter interface {
	applyDefaults(locs locations, path string)
}

// Script is one catalog entry.
type Script struct {
	ID               string            `yaml:"id"`
	Description      string            `yaml:"description"`
	Path             string            `yaml:"path"`
	RunAs            string            `yaml:"runAs"`
	WorkingDirectory string            `yaml:"workingDirectory"`
	Timeout          time.Duration     `yaml:"timeout"`
	Concurrency      int               `yaml:"concurrency"`
	TerminationGrace time.Duration     `yaml:"terminationGrace"`
	Env              map[string]string `yaml:"env" sensitive:"true"` // Values can be secrets
	Parameters       []Parameter       `yaml:"parameters"`
}

// Parameter is one declared script parameter. With neither Arg nor Env it is
// positional, in declared order.
type Parameter struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"` // ParamString, ParamInteger or ParamBoolean
	Pattern   string   `yaml:"pattern"`
	Enum      []string `yaml:"enum"` // When set, a value must be one of these
	Sensitive bool     `yaml:"sensitive"`
	Arg       string   `yaml:"arg"`
	Env       string   `yaml:"env"`
}

// Section is a provider's configuration: the settings decoded with defaults
// applied, plus the node they came from.
type Section[T any] struct {
	Settings T

	node *yaml.Node
	file string
	path string
}

// Decode strictly decodes the section into out, which should hold the
// provider's defaults. Unknown keys and bad values are reported as an
// ErrorList with file, line and column. An absent section leaves out as is.
func (s Section[T]) Decode(out any) error {
	if s.node == nil {
		return nil
	}
	d := newDecoder(s.file)
	d.decode(s.node, out, s.path)
	if df, ok := out.(defaulter); ok {
		df.applyDefaults(d.locs, s.path)
	}
	return d.errs.err()
}

func (s *Section[T]) setNode(n *yaml.Node, file, path string) {
	s.node, s.file, s.path = n, file, path
}

func (s *Section[T]) settings() any { return &s.Settings }

// sectionTarget is how the strict decoder recognises a Section field.
type sectionTarget interface {
	setNode(n *yaml.Node, file, path string)
	settings() any
}

// Default returns the built-in defaults. deploy/nexusd.default.yaml spells
// out the same values.
func Default() Config {
	return Config{
		Listen: []string{"127.0.0.1:7443"},
		TLS:    TLS{Mode: TLSAuto, ExtraSANs: []string{}},
		Limits: Limits{
			MaxBodyBytes:       1 << 20,
			StorageBudgetBytes: 1 << 30,
			OutputCapBytes:     10 << 20,
		},
		Auth:  Auth{FailureLimitPerMinute: 10},
		Audit: Audit{RetentionDays: 90},
		MCP:   MCP{AllowedOrigins: []string{}},
		Providers: Providers{
			Docker: Section[DockerConfig]{Settings: DockerConfig{
				Socket:              "/run/docker.sock",
				ConfigDir:           "/root/.docker",
				UpdateHealthTimeout: 60 * time.Second,
				UpdateMaxRuntime:    time.Hour,
				Compose:             ComposeConfig{TrustedOwners: []string{}},
			}},
			Apt: Section[AptConfig]{Settings: AptConfig{
				LockWait:   300 * time.Second,
				MaxRuntime: 6 * time.Hour,
			}},
			Scripts: Section[ScriptsConfig]{Settings: ScriptsConfig{Catalog: []Script{}}},
			Nginx: Section[NginxConfig]{Settings: NginxConfig{
				IncludeSuffix:       ".conf",
				DisabledSuffix:      ".disabled",
				Service:             "nginx",
				TestTimeout:         30 * time.Second,
				StateTimeout:        30 * time.Second,
				ReloadVerifyTimeout: 15 * time.Second,
				MaxConfigBytes:      256 << 10,
			}},
			Nexusd: Section[NexusdConfig]{Settings: NexusdConfig{
				HealthTimeout: nexusdspec.DefaultHealthTimeout,
				Update: NexusdUpdateConfig{
					MaxUploadBytes: nexusdspec.DefaultMaxUploadBytes,
					TrustedKeys:    []string{},
				},
			}},
			Pihole: Section[PiholeConfig]{Settings: PiholeConfig{DNSFile: "/etc/dnsmasq.d/03-custom-dns-names.conf"}},
			Host: Section[HostProviderConfig]{Settings: HostProviderConfig{
				SampleInterval:  hostspec.DefaultSampleInterval,
				HistoryDuration: hostspec.DefaultHistoryDuration,
				SourceTimeout:   hostspec.DefaultSourceTimeout,
			}},
			Services: Section[ServicesConfig]{Settings: ServicesConfig{
				Control: []ServiceControl{},
				Reboot:  ServicesReboot{Timeout: servicesspec.DefaultRebootTimeout},
			}},
		},
	}
}

// Load reads, checks and validates the file at path. It is what startup,
// reload and validate-config all use. Running as root, a file that is not
// root-owned or that has any group or other permission bit is refused,
// because catalog env values can be secrets (NFR-4).
func Load(path string) (*Config, error) {
	return load(path, os.Geteuid())
}

func load(path string, euid int) (*Config, error) {
	data, err := readChecked(path, euid)
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// ReadFile reads the file at path with Load's ownership and mode check, and
// returns its bytes for Parse, Edit or Inspect. Load is ReadFile then Parse.
// A failure is an ErrorList naming the file.
func ReadFile(path string) ([]byte, error) {
	return readChecked(path, os.Geteuid())
}

// readChecked is ReadFile with the effective uid injected, for tests.
func readChecked(path string, euid int) ([]byte, error) {
	data, err := readOpened(path, euid)
	if err != nil {
		return nil, ErrorList{{File: path, Msg: err.Error()}}
	}
	return data, nil
}

// readOpened opens the file once and checks the opened descriptor, so the
// mode that is checked belongs to the bytes that are read.
func readOpened(path string, euid int) (data []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", unwrapPath(err))
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("cannot read configuration: %w", cerr)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", unwrapPath(err))
	}
	if err := checkFile(path, fi, euid); err != nil {
		return nil, err
	}
	data, err = io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", unwrapPath(err))
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("configuration is larger than %d bytes", maxFileBytes)
	}
	return data, nil
}

// unwrapPath drops the *fs.PathError wrapper because the file is already
// named at the start of every reported error.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// checkFile enforces ownership and mode when running as root. Not running as
// root it checks only that the path is a regular file.
func checkFile(path string, fi fs.FileInfo, euid int) error {
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if euid != 0 {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine the file owner")
	}
	if st.Uid != 0 {
		return fmt.Errorf("owned by uid %d, must be owned by root; run chown root:root %s && chmod 0600 %s", st.Uid, path, path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("mode %04o gives group or other access and the file can hold secrets; run chmod 0600 %s", perm, path)
	}
	return nil
}

// Parse decodes and validates configuration bytes. file names the source in
// errors. An empty document yields the defaults.
func Parse(data []byte, file string) (*Config, error) {
	cfg := Default()
	cfg.file = file
	d := newDecoder(file)

	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	switch err := dec.Decode(&doc); {
	case errors.Is(err, io.EOF):
		// Empty file: defaults only.
	case err != nil:
		return nil, ErrorList{syntaxError(file, err)}
	default:
		var extra yaml.Node
		switch err := dec.Decode(&extra); {
		case errors.Is(err, io.EOF):
		case err != nil:
			return nil, ErrorList{syntaxError(file, err)}
		default:
			line, col := extra.Line, extra.Column
			if len(extra.Content) > 0 {
				line, col = extra.Content[0].Line, extra.Content[0].Column
			}
			d.errs.add(line, col, "", "the file must hold a single YAML document")
		}
		if len(doc.Content) > 0 {
			d.decode(doc.Content[0], &cfg, "")
		}
	}

	if cfg.Host.Name == "" {
		name, err := os.Hostname()
		if err != nil {
			d.errs.add(0, 0, "host.name", "empty and the OS hostname is unavailable: "+err.Error())
		}
		cfg.Host.Name = name
	}
	cfg.Providers.Services.Settings.applyDefaults(d.locs, "providers.services")
	cfg.locs = d.locs
	cfg.validate(&d.errs)
	if err := d.errs.err(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var syntaxLine = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)

func syntaxError(file string, err error) *Error {
	msg := err.Error()
	if m := syntaxLine.FindStringSubmatch(msg); m != nil {
		if line, err := strconv.Atoi(m[1]); err == nil {
			return &Error{File: file, Line: line, Msg: "invalid YAML: " + m[2]}
		}
	}
	return &Error{File: file, Msg: "invalid YAML: " + strings.TrimPrefix(msg, "yaml: ")}
}

// Error is one located configuration problem. Line and Column are 1-based
// and zero when unknown; Path is the dotted key path, such as
// providers.scripts.catalog[0].parameters[1].pattern.
type Error struct {
	File   string
	Line   int
	Column int
	Path   string
	Msg    string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Line > 0 {
		b.WriteString(":" + strconv.Itoa(e.Line))
		if e.Column > 0 {
			b.WriteString(":" + strconv.Itoa(e.Column))
		}
	}
	b.WriteString(": ")
	if e.Path != "" {
		b.WriteString(e.Path + ": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// ErrorList is every problem found in one file, ordered by position.
type ErrorList []*Error

func (l ErrorList) Error() string {
	lines := make([]string, len(l))
	for i, e := range l {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// collector accumulates errors for one file.
type collector struct {
	file string
	list ErrorList
}

func (c *collector) add(line, col int, path, msg string) {
	c.list = append(c.list, &Error{File: c.file, Line: line, Column: col, Path: path, Msg: msg})
}

func (c *collector) addf(line, col int, path, format string, args ...any) {
	c.add(line, col, path, fmt.Sprintf(format, args...))
}

// err returns the errors sorted by position, unlocated ones last, or nil.
func (c *collector) err() error {
	if len(c.list) == 0 {
		return nil
	}
	sort.SliceStable(c.list, func(i, j int) bool {
		a, b := c.list[i], c.list[j]
		if (a.Line == 0) != (b.Line == 0) {
			return b.Line == 0
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return c.list
}
