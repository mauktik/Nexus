// Package spec declares the nexusd provider's operations and wire types: the
// daemon managing itself on its own host. It holds only data and types, so
// the CLI can import it through internal/catalog without linking the
// provider (CON-5, D-1).
//
// No input carries a path, a command or a URL (NSM FR-25): the daemon acts
// only on its own binary, unit and configuration file, and an update reaches
// the host as an uploaded, signed release tarball named by an opaque,
// server-generated upload ID. Trusted release keys are set only in the
// root-owned configuration file: no operation adds or changes one (FR-15),
// and config set denies providers.nexusd.update.trustedKeys
// (nexusd-remote-admin FR-12).
//
// The token ops manage the daemon's own bearer tokens remotely (list, add,
// remove), each requiring nexusd:admin. Add returns the new secret once, in
// its response only (nexusd-remote-admin FR-1 to FR-3).
//
// The config reads (list, get) return the keys of the on-disk configuration
// file by dotted key path, a path in the config schema and never a file
// path, with secret values redacted (nexusd-remote-admin FR-6).
package spec

import (
	"strconv"
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
	TokensType  = "tokens"
	ConfigType  = "config"
)

// Remote token limits (ASM-6; A-1, A-2). MaxRemoteTokens caps the tokens a
// host holds, local ones included, and only a remote add enforces it.
// TokenMutationsPerMinute bounds successful remote adds and removes
// together, per daemon process, over a rolling minute.
const (
	MaxRemoteTokens         = 256
	TokenMutationsPerMinute = 10
)

// MaxConfigValueBytes bounds one configuration value set remotely (NFR-5).
// internal/config's Edit enforces it.
const MaxConfigValueBytes = 4096

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

// TokenListInput is the input of nexusd.tokens.list.
type TokenListInput struct{}

// Token is one bearer token of the host as nexusd.tokens.list returns it.
// Its name is the token name; it never carries the secret or its hash.
type Token = apitypes.Resource[TokenProperties]

// TokenProperties describe a stored token.
type TokenProperties struct {
	TokenID   string     `json:"tokenId"` // 12 lowercase hex digits
	Scopes    []string   `json:"scopes"`
	Created   time.Time  `json:"created"`
	Expires   *time.Time `json:"expires"`             // Null: never
	Self      bool       `json:"self"`                // The token this request used
	CreatedBy *TokenRef  `json:"createdBy,omitempty"` // The token that added it remotely
}

// TokenRef names a token by ID and name.
type TokenRef struct {
	TokenID string `json:"tokenId"`
	Name    string `json:"name"`
}

// TokenAddInput is the input of nexusd.tokens.add.
type TokenAddInput struct {
	Name    string   `json:"name" jsonschema:"Name of the new token: 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or digit."`
	Scopes  []string `json:"scopes,omitempty" jsonschema:"Scopes the token holds, such as containers:read or scripts:run:backup. Default none."`
	Expires string   `json:"expires,omitempty" jsonschema:"Lifetime: a Go duration such as 1h30m, or days such as 90d. Default: never expires."`
}

// TokenAddResult is the result of nexusd.tokens.add. Token is the secret,
// returned once (def.Op.SecretOutput).
type TokenAddResult struct {
	apitypes.ActionResult
	Token   string     `json:"token"`
	TokenID string     `json:"tokenId"`
	Name    string     `json:"name"`
	Scopes  []string   `json:"scopes"`
	Created time.Time  `json:"created"`
	Expires *time.Time `json:"expires"` // Null: never
}

// TokenRemoveInput is the input of nexusd.tokens.remove.
type TokenRemoveInput struct {
	Name string `json:"name" jsonschema:"Name or 12-hex ID of the token to revoke."`
}

// TokenRemoveResult is the result of nexusd.tokens.remove. Its warnings say
// when the revoked token is the one the request used.
type TokenRemoveResult struct {
	apitypes.ActionResult
	TokenID     string `json:"tokenId"`
	Name        string `json:"name"`
	RevokedSelf bool   `json:"revokedSelf"`
}

// ConfigListInput is the input of nexusd.config.list.
type ConfigListInput struct{}

// ConfigGetInput is the input of nexusd.config.get.
type ConfigGetInput struct {
	Name string `json:"name" jsonschema:"Dotted key of the configuration schema, such as providers.host.enabled, or a section such as providers.host."`
}

// ConfigKey is one configuration key as nexusd.config.list and get return
// it. Its name is the dotted key, such as providers.host.enabled.
type ConfigKey = apitypes.Resource[ConfigKeyProperties]

// ConfigKeyProperties are a key's value in the on-disk file, never the
// configuration in force. A sensitive value is config.Redacted.
type ConfigKeyProperties struct {
	Value     any    `json:"value"`     // As written in the file; null when absent
	Effective any    `json:"effective"` // What the file loads as, the default included
	Source    string `json:"source"`    // file or default
}

// configKeyPattern matches a dotted key of the config schema: 1 to 8
// segments, each a lowercase letter then up to 63 letters or digits. It
// never matches a file path ("/", "..", a leading or trailing dot).
const configKeyPattern = `^[a-z][A-Za-z0-9]{0,63}(\.[a-z][A-Za-z0-9]{0,63}){0,7}$`

func refineConfigName(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = configKeyPattern
	}
}

// Token input patterns. tokenNamePattern and tokenScopePattern equal
// internal/auth's nameRE and scopeRE (a test pins them); a name also
// matches a 12-hex token ID, so remove takes either. tokenExpiresPattern
// accepts what the local nexusd token command does: days from 1 to 99999,
// or a non-negative Go duration.
const (
	tokenNamePattern    = `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`
	tokenScopePattern   = `^[a-z][a-z0-9-]*:[a-z][a-z0-9-]*(:[A-Za-z0-9][A-Za-z0-9._-]*)?$`
	tokenExpiresPattern = `^([1-9][0-9]{0,4}d|([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+)$`
	tokenExpiresMaxLen  = 32
	tokenMaxScopes      = 32
)

func refineTokenName(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = tokenNamePattern
	}
}

func refineTokenAdd(s *jsonschema.Schema) {
	refineTokenName(s)
	if p := s.Properties["scopes"]; p != nil && p.Items != nil {
		n := tokenMaxScopes
		p.MaxItems, p.Items.Pattern = &n, tokenScopePattern
	}
	if p := s.Properties["expires"]; p != nil {
		n := tokenExpiresMaxLen
		p.Pattern, p.MaxLength = tokenExpiresPattern, &n
	}
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
	// tokenSyncNote says what a token mutation's answer is.
	tokenSyncNote = " It answers 200 at once and creates no operation; an Idempotency-Key header is refused " +
		"with validation-failed."
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

// tokenLimitNote is FR-5's rate limit, shared by add and remove.
var tokenLimitNote = " Successful token adds and removes together are limited to " +
	strconv.Itoa(TokenMutationsPerMinute) + " per minute per daemon; past that the call answers 429 " +
	"rate-limited with Retry-After and changes nothing."

// The remote token ops (nexusd-remote-admin FR-1 to FR-3). None is an MCP
// tool: an agent manages tokens over REST or the CLI only.
var (
	TokensList = def.Define[TokenListInput, apitypes.List[Token]](def.Op{
		ID: Name + ".tokens.list", Provider: Name, Kind: def.KindList, ResourceType: TokensType,
		Summary: "List nexusd's bearer tokens",
		Description: "Lists every bearer token of this host, those made by the local nexusd token command " +
			"included: each token's name, tokenId, scopes, created, expires (null: never), createdBy (the token " +
			"that added it remotely, absent otherwise), and self, true for the token this request used. It never " +
			"returns a secret or a hash. Read-only, but audited: each call writes a requested and a completed " +
			"audit record." + adminNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		Audited: true,
		CLI:     def.CLIBinding{Noun: "nexusdtoken", Verb: "get"},
		Columns: []string{"name", "properties.tokenId", "properties.scopes", "properties.expires", "properties.self"},
	})

	TokensAdd = def.Define[TokenAddInput, TokenAddResult](def.Op{
		ID: Name + ".tokens.add", Provider: Name, Kind: def.KindResourceAction, ResourceType: TokensType,
		Action:  "add",
		Summary: "Add a nexusd bearer token",
		Description: "Creates a bearer token named name with the given scopes (none by default) that expires " +
			"after expires (such as 90d or 1h30m; never by default), and answers with the secret token, " +
			"its tokenId, name, scopes, created and expires. The secret is returned once: it is never stored, " +
			"logged or shown again, so keep it from this response. Side effects: the token is added to the " +
			"host's token store and authenticates from the next request; the audit log records its name, ID, " +
			"scopes and expiry, never the secret. A name already taken answers 409 conflict. A host holds at " +
			"most " + strconv.Itoa(MaxRemoteTokens) + " tokens, local ones included: past that the call " +
			"answers 409 token-limit-reached with maxTokens." + tokenLimitNote + tokenSyncNote + adminNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskMutating, Mode: def.ModeSync,
		SyncMutation: true, SecretOutput: "token",
		CLI:     def.CLIBinding{Noun: "nexusdtoken", Verb: "add", Positional: "name"},
		Columns: []string{"name", "tokenId", "token", "expires"},
		Refine:  refineTokenAdd,
	})

	TokensRemove = def.Define[TokenRemoveInput, TokenRemoveResult](def.Op{
		ID: Name + ".tokens.remove", Provider: Name, Kind: def.KindResourceAction, ResourceType: TokensType,
		Action:  "remove",
		Summary: "Revoke a nexusd bearer token",
		Description: "Revokes the token whose name or tokenId is name, and answers with its tokenId and name, " +
			"and revokedSelf true when it is the token this request used; the warnings then say this client can " +
			"no longer authenticate to this host with it. Side effects: the token is deleted from the host's " +
			"token store and is refused from the next request. An unknown name or ID answers 404 not-found and " +
			"changes nothing." + tokenLimitNote + tokenSyncNote + adminNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskDestructive, Mode: def.ModeSync,
		SyncMutation: true,
		CLI:          def.CLIBinding{Noun: "nexusdtoken", Verb: "remove", Positional: "name", EchoWarnings: true},
		Refine:       refineTokenName,
	})
)

// configReadNote is what both config reads say about their values and
// errors (FR-6).
const configReadNote = " Values are read from the on-disk configuration file, not the configuration in force, " +
	"which can differ until a reload or restart applies the file. A secret value, such as a script's env, is " +
	"[REDACTED]. Fails config-invalid with located errors when the file cannot be read or parsed. Read-only, " +
	"but audited: each call writes a requested and a completed audit record." + adminNote

var configColumns = []string{"name", "properties.value", "properties.effective", "properties.source"}

// The config reads (nexusd-remote-admin FR-6). Neither is an MCP tool.
var (
	ConfigList = def.Define[ConfigListInput, apitypes.List[ConfigKey]](def.Op{
		ID: Name + ".config.list", Provider: Name, Kind: def.KindList, ResourceType: ConfigType,
		Summary: "List nexusd's configuration keys",
		Description: "Lists every leaf key of nexusd's configuration schema, in schema order, by dotted name such as " +
			"providers.host.enabled: its value as written in the on-disk file (null when absent), its effective " +
			"value (what the file loads as, the default when absent) and its source, file or default. An " +
			"unknown key named to nexusd.config.get answers 404 not-found naming the nearest valid parent and " +
			"its keys." + configReadNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		Audited: true,
		CLI:     def.CLIBinding{Noun: "nexusdconfig", Verb: "get"},
		Columns: configColumns,
	})

	ConfigGet = def.Define[ConfigGetInput, ConfigKey](def.Op{
		ID: Name + ".config.get", Provider: Name, Kind: def.KindGet, ResourceType: ConfigType,
		Summary: "Get a nexusd configuration key",
		Description: "Returns one key of nexusd's configuration by dotted name, such as providers.host.enabled: " +
			"its value as written in the on-disk file (null when absent), its effective value (what the file " +
			"loads as, the default when absent) and its source, file or default. A section, such as " +
			"providers.host, returns its keys' values together. An unknown key answers 404 not-found whose " +
			"detail names the nearest valid parent and its keys." + configReadNote,
		Scope: def.Scope{Name: AdminScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		Audited: true,
		CLI:     def.CLIBinding{Noun: "nexusdconfig", Verb: "get", Positional: "name"},
		Columns: configColumns,
		Refine:  refineConfigName,
	})
)

// Spec returns the nexusd provider's declarations for the catalog.
func Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: Name, ReadScope: ReadScope, Ops: []def.Op{
		List.Op(), ValidateConfig.Op(), Reload.Op(), Restart.Op(), Update.Op(), Rollback.Op(), Upload.Op(),
		TokensList.Op(), TokensAdd.Op(), TokensRemove.Op(), ConfigList.Op(), ConfigGet.Op(),
	}}
}
