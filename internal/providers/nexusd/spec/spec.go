// Package spec declares the nexusd provider's operations and wire types: the
// daemon managing itself on its own host. It holds only data and types, so
// the CLI can import it through internal/catalog without linking the
// provider (CON-5, D-1).
//
// No input carries a path, a command or a URL (NSM FR-25): the daemon acts
// only on its own binary, unit and configuration file, and an update reaches
// the host as an uploaded, signed release tarball named by an opaque,
// server-generated upload ID. Trusted release keys are set only in the
// root-owned configuration file; no operation adds or changes one (FR-15).
package spec

import (
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
)

// Provider identity, resource types and scopes (FR-1). AdminScope is
// root-equivalent: it installs and runs a new daemon binary.
const (
	Name        = "nexusd"
	ReadScope   = "nexusd:read"
	AdminScope  = "nexusd:admin"
	DaemonsType = "daemons"
	UploadsType = "uploads"
)

// LaneKey is the lane reload, restart, update and rollback share with the
// reject policy, so only one of them is queued or running at a time (FR-11).
const LaneKey = "nexusd"

// Upload size bounds: the default and the ceiling of
// providers.nexusd.update.maxUploadBytes (FR-6).
const (
	DefaultMaxUploadBytes = 64 << 20
	MaxUploadBytesCeiling = 256 << 20
)

// DefaultHealthTimeout is the default of providers.nexusd.healthTimeout, the
// longest the FR-10 health check polls (ASM-4).
const DefaultHealthTimeout = 30 * time.Second

// ReconnectWindow is how long a waited restart, update or rollback command
// keeps reconnecting to a restarting daemon (FR-14).
const ReconnectWindow = 60 * time.Second

// UploadIDPattern matches the server-generated upload ID: 128 random bits as
// lowercase hex. It is a file name under staging/, so it never holds "/" or
// "..".
const UploadIDPattern = `^[0-9a-f]{32}$`

// SignaturePattern matches a 64-byte ed25519 signature in standard, padded
// base64: 86 characters and "==" (RFC 4648 section 4; 64 bytes encode to 88
// characters with two padding characters).
const SignaturePattern = `^[A-Za-z0-9+/]{86}==$`

const signatureLength = 88

// ListInput is the input of nexusd.daemons.list: the host's one daemon.
type ListInput struct{}

// Daemon is the running nexusd as the API returns it. Its id is
// /providers/nexusd/daemons/nexusd.
type Daemon = apitypes.Resource[DaemonProperties]

// DaemonProperties describe the running daemon and its rollback binary
// (FR-2).
type DaemonProperties struct {
	Version        string    `json:"version"`
	StartedAt      time.Time `json:"startedAt"`
	UptimeSeconds  int64     `json:"uptimeSeconds"`
	ConfigPath     string    `json:"configPath"`     // The path the daemon was started with
	ExecutablePath string    `json:"executablePath"` // The running executable
	Previous       Previous  `json:"previous"`       // The rollback binary
	// UpdateConfigured is true when at least one trusted release key is
	// configured, so update can verify a package (FR-15).
	UpdateConfigured bool         `json:"updateConfigured"`
	SelfManagement   Availability `json:"selfManagement"` // Whether restart, update and rollback can run
}

// Previous is the rollback binary, nexusd.prev.
type Previous struct {
	Present bool   `json:"present"`
	Path    string `json:"path"`
	// Version is what the binary prints for -version, or "unknown" when that
	// fails or takes more than 5 seconds.
	Version string `json:"version"`
}

// Availability reports whether a capability works on this host, and why
// not when it does not.
type Availability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// ValidateInput is the input of nexusd.actions.validate-config: the daemon
// validates its own configuration file, never a path the caller names.
type ValidateInput struct{}

// ValidateResult is the result of nexusd.actions.validate-config. An invalid
// file is a successful call with valid false (FR-3).
type ValidateResult struct {
	Host       string        `json:"host"`
	Valid      bool          `json:"valid"`
	ConfigPath string        `json:"configPath"`
	Errors     []ConfigError `json:"errors"`
}

// ConfigError is one configuration problem, located when the location is
// known.
type ConfigError struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Path    string `json:"path,omitempty"` // Dotted key path, e.g. providers.nexusd.healthTimeout
	Message string `json:"message"`
}

// ReloadInput is the input of nexusd.actions.reload.
type ReloadInput struct{}

// ReloadResult is the result of nexusd.actions.reload.
type ReloadResult struct {
	apitypes.ActionResult
	Applied bool `json:"applied"`
	// RestartRequired are the top-level keys whose change takes effect only
	// after a restart.
	RestartRequired []string `json:"restartRequired"`
}

// RestartInput is the input of nexusd.actions.restart.
type RestartInput struct{}

// RestartResult is the result of nexusd.actions.restart.
type RestartResult struct {
	apitypes.ActionResult
	Version string `json:"version"` // The version running after the restart
}

// UpdateInput is the input of nexusd.actions.update. It names an upload,
// never a file, command or URL.
type UpdateInput struct {
	UploadID       string `json:"uploadId" jsonschema:"ID of a staged release upload, from POST /providers/nexusd/uploads."`
	AllowDowngrade bool   `json:"allowDowngrade,omitempty" jsonschema:"Install a package older than the running version. Default false."`
}

// UpdateResult is the result of nexusd.actions.update.
type UpdateResult struct {
	apitypes.ActionResult
	PreviousVersion string `json:"previousVersion"`
	NewVersion      string `json:"newVersion"`
	RolledBack      bool   `json:"rolledBack"` // Always false on success
}

// RollbackInput is the input of nexusd.actions.rollback.
type RollbackInput struct{}

// RollbackResult is the result of nexusd.actions.rollback.
type RollbackResult struct {
	apitypes.ActionResult
	FromVersion string `json:"fromVersion"`
	ToVersion   string `json:"toVersion"`
}

// UploadInput is the query input of nexusd.uploads.add; the request body is
// the release tarball.
type UploadInput struct {
	Signature string `json:"signature" jsonschema:"Detached ed25519 signature of the tarball, standard base64 (the .sig file's content)."`
}

// UploadResult is the 201 result of nexusd.uploads.add.
type UploadResult struct {
	UploadID  string    `json:"uploadId"`
	SHA256    string    `json:"sha256"` // Lowercase hex digest of the body
	SizeBytes int64     `json:"sizeBytes"`
	ExpiresAt time.Time `json:"expiresAt"` // When the staged upload is deleted if no update used it
}

func refineUpdate(s *jsonschema.Schema) {
	if p := s.Properties["uploadId"]; p != nil {
		p.Pattern = UploadIDPattern
	}
}

func refineUpload(s *jsonschema.Schema) {
	if p := s.Properties["signature"]; p != nil {
		n := signatureLength
		p.Pattern, p.MaxLength = SignaturePattern, &n
	}
}

// Description fragments shared by the lane ops.
const (
	adminNote = " Requires nexusd:admin, which is root-equivalent."
	laneNote  = " Reload, restart, update and rollback share one lane: a second request while one is queued or " +
		"running answers 409 naming the blocking operation. It can be canceled only while queued."
	// interruptNote is FR-11's statement of what a daemon restart does to
	// other providers' operations.
	interruptNote = " Restarting nexusd neither waits for nor cancels other operations: in-process operations " +
		"of other providers end outcome-unknown, and detached operations survive."
	recordNote = " It runs in a systemd unit (systemd hosts only, not OpenRC) outside the sandbox, and the daemon that is running when the " +
		"unit ends records the outcome. The CLI keeps reconnecting for up to 60 seconds while nexusd restarts."
)

// The nexusd operations, in the order of the Operation catalog table.
var (
	List = def.Define[ListInput, apitypes.List[Daemon]](def.Op{
		ID: Name + ".daemons.list", Provider: Name, Kind: def.KindList, ResourceType: DaemonsType,
		Summary: "Get the running nexusd",
		Description: "Returns this host's nexusd as a one-item list: its version, start time and uptime, the " +
			"configuration file and executable it runs from, the rollback binary nexusd.prev (present, path and " +
			"the version it reports, or unknown), whether release keys are configured for update, and whether " +
			"self-management (restart, update, rollback) is available, with the reason when not. Read-only.",
		Scope: def.Scope{Name: ReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI: def.CLIBinding{Noun: Name, Verb: "get"},
		MCP: &def.MCPBinding{Tool: "get_nexusd", Description: "Get the running nexusd's version, uptime, config path and rollback binary."},
		Columns: []string{
			"name", "properties.version", "properties.uptimeSeconds", "properties.previous.version",
			"properties.updateConfigured",
		},
	})

	ValidateConfig = def.Define[ValidateInput, ValidateResult](def.Op{
		ID: Name + ".actions.validate-config", Provider: Name, Kind: def.KindProviderAction,
		Action:  "validate-config",
		Summary: "Validate the nexusd configuration file",
		Description: "Validates the configuration file nexusd was started with, on disk now, with exactly the " +
			"checks of nexusd validate-config, and returns valid and each error with its file, line, column and " +
			"key path when known. An invalid file is a successful call with valid false. It never applies the " +
			"file and takes no path. Read-only.",
		Scope: def.Scope{Name: ReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI: def.CLIBinding{Noun: Name, Verb: "validate-config"},
		MCP: &def.MCPBinding{Tool: "validate_nexusd_config", Description: "Check nexusd's on-disk config file without applying it."},
	})

	Reload = def.Define[ReloadInput, ReloadResult](def.Op{
		ID: Name + ".actions.reload", Provider: Name, Kind: def.KindProviderAction, Action: "reload",
		Summary: "Reload the nexusd configuration",
		Description: "Re-reads and applies nexusd's configuration file, as SIGHUP does, serialized with SIGHUP. " +
			"An invalid file is refused with 422 config-invalid and its errors, and no operation is created. " +
			"Side effects: the new configuration is applied, all or nothing: if a provider refuses its section " +
			"the operation fails with config-apply-failed and the configuration in force is unchanged. The " +
			"result lists restartRequired, the top-level keys that take effect only after a restart. " +
			"Long-running: answers 202 with an operation." + laneNote + adminNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		Cancel: def.CancelQueuedOnly,
		CLI:    def.CLIBinding{Noun: Name, Verb: "reload"},
	})

	Restart = def.Define[RestartInput, RestartResult](def.Op{
		ID: Name + ".actions.restart", Provider: Name, Kind: def.KindProviderAction, Action: "restart",
		Summary: "Restart nexusd",
		Description: "Restarts nexusd.service and checks its health: active, running the expected version, " +
			"answering /healthz, and still so 5 seconds later, within providers.nexusd.healthTimeout. Side " +
			"effects: the daemon stops and starts; the API is unreachable meanwhile. Unhealthy fails with " +
			"restart-unhealthy, without rollback. Long-running: answers 202 with an operation." + recordNote +
			interruptNote + laneNote + adminNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		Cancel: def.CancelQueuedOnly,
		CLI:    def.CLIBinding{Noun: Name, Verb: "restart", ReconnectWindow: ReconnectWindow},
	})

	Update = def.Define[UpdateInput, UpdateResult](def.Op{
		ID: Name + ".actions.update", Provider: Name, Kind: def.KindProviderAction, Action: "update",
		Summary: "Update nexusd from a signed release",
		Description: "Installs the release tarball staged as uploadId. Before any operation exists it is refused " +
			"with update-not-configured (no trusted key), upload-not-found, update-signature-invalid, " +
			"update-package-invalid, or 409 version-downgrade (an older version without allowDowngrade); the " +
			"same version succeeds with changed false. Side effects: the current binary and unit file are saved " +
			"as the rollback copy, the package's install.sh runs as root, and nexusd restarts once onto the new " +
			"version. If install, restart or the health check fails, the previous binary is restored and the " +
			"operation fails with update-failed and rolledBack true, or update-rollback-failed if the restore " +
			"also fails. Long-running: answers 202 with an operation." + recordNote + interruptNote + laneNote +
			adminNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		Cancel: def.CancelQueuedOnly,
		CLI: def.CLIBinding{Noun: Name, Verb: "update", ReconnectWindow: ReconnectWindow,
			Upload: &def.CLIUpload{
				Op: Name + ".uploads.add", Field: "uploadId", FileFlag: "file",
				FileUsage: "Release tarball to install, nexusd-<version>-linux-amd64.tar.gz.",
				Sidecars: []def.CLISidecar{{
					Param: "signature", Flag: "signature", Suffix: ".sig", MaxBytes: 4096,
					Usage: "Signature file of the tarball. Default <file>.sig.",
				}},
			}},
		Refine: refineUpdate,
	})

	Rollback = def.Define[RollbackInput, RollbackResult](def.Op{
		ID: Name + ".actions.rollback", Provider: Name, Kind: def.KindProviderAction, Action: "rollback",
		Summary: "Roll nexusd back to the previous binary",
		Description: "Swaps nexusd with the rollback binary nexusd.prev, and the unit file with its saved copy " +
			"when there is one, so a rollback can itself be rolled back. Refused with rollback-unavailable when " +
			"there is no nexusd.prev, and with config-invalid when it rejects the current configuration. Side " +
			"effects: the installed binary changes and nexusd restarts onto the previous version; if that is " +
			"unhealthy the swap is undone and the operation fails with rollback-failed. Long-running: answers " +
			"202 with an operation." + recordNote + interruptNote + laneNote + adminNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskDestructive, Mode: def.ModeLongRunning,
		Cancel: def.CancelQueuedOnly,
		CLI:    def.CLIBinding{Noun: Name, Verb: "rollback", ReconnectWindow: ReconnectWindow},
	})

	// Upload has no command: the CLI reaches it only through update's
	// CLI.Upload binding.
	Upload = def.Define[UploadInput, UploadResult](def.Op{
		ID: Name + ".uploads.add", Provider: Name, Kind: def.KindUpload, ResourceType: UploadsType,
		Summary: "Stage a release tarball for update",
		Description: "Stages a release tarball, the request body, with its detached signature for a later " +
			"nexusd update, and answers 201 with uploadId, sha256, sizeBytes and expiresAt. Side effects: the " +
			"body is stored on the host, never in memory whole, and deleted an hour later or when the update " +
			"that used it ends; at most 2 are staged at once, and a body that matches neither answers 409 once " +
			"received, while re-sending a staged upload (same bytes and signature) answers 201 with its " +
			"existing uploadId. A body over " +
			"providers.nexusd.update.maxUploadBytes answers 413 upload-too-large and nothing is kept. The " +
			"signature is verified by update, not here." + adminNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskMutating, Mode: def.ModeSync,
		Upload: &def.Upload{
			ContentTypes: []string{"application/octet-stream", "application/gzip"},
			MaxBytes:     MaxUploadBytesCeiling,
		},
		Refine: refineUpload,
	})
)

// Spec returns the nexusd provider's declarations for the catalog.
func Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: Name, ReadScope: ReadScope, Ops: []def.Op{
		List.Op(), ValidateConfig.Op(), Reload.Op(), Restart.Op(), Update.Op(), Rollback.Op(), Upload.Op(),
	}}
}
