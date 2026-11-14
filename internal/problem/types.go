package problem

import (
	"slices"
	"strings"
)

// Slug names a problem type. Its type URI is urn:nexus:problem:<slug>.
type Slug string

// HTTP-level problem types.
const (
	ValidationFailed      Slug = "validation-failed"
	Unauthorized          Slug = "unauthorized"
	Forbidden             Slug = "forbidden"
	NotFound              Slug = "not-found"
	RouteNotFound         Slug = "route-not-found"
	MethodNotAllowed      Slug = "method-not-allowed"
	Conflict              Slug = "conflict"
	NotCancelable         Slug = "not-cancelable"
	PayloadTooLarge       Slug = "payload-too-large"
	NotUpdatable          Slug = "not-updatable"
	ScriptIntegrityFailed Slug = "script-integrity-failed"
	ComposeFilesMissing   Slug = "compose-files-missing"
	IdempotencyKeyReused  Slug = "idempotency-key-reused"
	RateLimited           Slug = "rate-limited"
	InternalError         Slug = "internal-error"
	CapabilityUnavailable Slug = "capability-unavailable"
	ComposePluginMissing  Slug = "compose-plugin-missing"
	DependencyUnavailable Slug = "dependency-unavailable"
	AuditUnavailable      Slug = "audit-unavailable"
	StorageLimitReached   Slug = "storage-limit-reached"

	// nexusd self-management (nexusd-self-management architecture.md,
	// Problem types).
	ConfigInvalid          Slug = "config-invalid"
	UploadTooLarge         Slug = "upload-too-large"
	UploadNotFound         Slug = "upload-not-found"
	UpdateNotConfigured    Slug = "update-not-configured"
	UpdateSignatureInvalid Slug = "update-signature-invalid"
	UpdatePackageInvalid   Slug = "update-package-invalid"
	VersionDowngrade       Slug = "version-downgrade"
	RollbackUnavailable    Slug = "rollback-unavailable"

	// pihole-dns-zones (pihole-dns-zones architecture.md, Problem types).
	DNSFileConflict  Slug = "dns-file-conflict"
	DNSNameUnzoned   Slug = "dns-name-unzoned"
	DNSBatchRejected Slug = "dns-batch-rejected"

	// nginx-provider (nginx-provider architecture.md, Problem types).
	NginxConfigInvalid   Slug = "nginx-config-invalid"
	NginxConfigUnmanaged Slug = "nginx-config-unmanaged"
	NginxNotRunning      Slug = "nginx-not-running"
)

// Operation-level problem types: they appear only inside operation.error.
const (
	OutcomeUnknown       Slug = "outcome-unknown"
	PackageManagerBusy   Slug = "package-manager-busy"
	PackageManagerBroken Slug = "package-manager-broken"
	AptFailed            Slug = "apt-failed"
	ScriptFailed         Slug = "script-failed"
	ScriptTimeout        Slug = "script-timeout"
	UpdateHealthFailed   Slug = "update-health-failed"

	// nexusd self-management.
	ConfigApplyFailed    Slug = "config-apply-failed"
	RestartUnhealthy     Slug = "restart-unhealthy"
	UpdateFailed         Slug = "update-failed"
	UpdateRollbackFailed Slug = "update-rollback-failed"
	RollbackFailed       Slug = "rollback-failed"

	// pihole-dns-zones.
	DNSConfigTestFailed   Slug = "dns-config-test-failed"
	DNSVerificationFailed Slug = "dns-verification-failed"
	DNSRollbackFailed     Slug = "dns-rollback-failed"
	GravityFailed         Slug = "gravity-failed"
	PiholeCommandFailed   Slug = "pihole-command-failed"
	PiholeBusy            Slug = "pihole-busy"
	NginxReloadFailed     Slug = "nginx-reload-failed"
)

// Type is one registered problem type. Problem types are part of the
// versioned API contract (FR-8).
type Type struct {
	Slug           Slug
	Title          string
	Status         int      // HTTP status; 0 for operation-level types
	Exit           int      // CLI exit code
	Extensions     []string // Body members beyond the RFC 9457 standard ones
	OperationLevel bool     // Appears only inside operation.error
}

// URI is the type's stable identifier, with no dependency on a domain (CON-9).
func (t Type) URI() string { return TypePrefix + string(t.Slug) }

// TypePrefix is the URN namespace of every problem type.
const TypePrefix = "urn:nexus:problem:"

// registry is the problem types table from architecture.md (Interfaces,
// Problem types). It is the single place a problem type may be defined: New
// refuses any slug not listed here.
var registry = []Type{
	{Slug: ValidationFailed, Title: "Validation failed", Status: 400, Exit: 2, Extensions: []string{"errors"}},
	{Slug: Unauthorized, Title: "Unauthorized", Status: 401, Exit: 3},
	{Slug: Forbidden, Title: "Forbidden", Status: 403, Exit: 3, Extensions: []string{"requiredScope"}},
	{Slug: NotFound, Title: "Not found", Status: 404, Exit: 4},
	{Slug: RouteNotFound, Title: "Route not found", Status: 404, Exit: 4},
	{Slug: MethodNotAllowed, Title: "Method not allowed", Status: 405, Exit: 2},
	{Slug: Conflict, Title: "Conflict", Status: 409, Exit: 1, Extensions: []string{"blockingOperationId"}},
	{Slug: NotCancelable, Title: "Not cancelable", Status: 409, Exit: 1, Extensions: []string{"operationId"}},
	{Slug: PayloadTooLarge, Title: "Payload too large", Status: 413, Exit: 2},
	{Slug: NotUpdatable, Title: "Not updatable", Status: 422, Exit: 1, Extensions: []string{"reason", "hint"}},
	{Slug: ScriptIntegrityFailed, Title: "Script integrity check failed", Status: 422, Exit: 1, Extensions: []string{"check", "path"}},
	{Slug: ComposeFilesMissing, Title: "Compose files missing", Status: 422, Exit: 1, Extensions: []string{"path"}},
	{Slug: IdempotencyKeyReused, Title: "Idempotency key reused", Status: 422, Exit: 1, Extensions: []string{"operationId"}},
	// nginx -t runs only in detached operations (CON-2), so nginx-config-invalid
	// surfaces inside operation.error even though it carries an HTTP status.
	{Slug: NginxConfigInvalid, Title: "nginx configuration invalid", Status: 422, Exit: 1, Extensions: []string{"testOutput", "rolledBack"}},
	{Slug: NginxConfigUnmanaged, Title: "nginx configuration not managed", Status: 422, Exit: 1, Extensions: []string{"reason", "fileName"}},
	{Slug: NginxNotRunning, Title: "nginx not running", Status: 422, Exit: 1, Extensions: []string{"state"}},
	// rate-limited carries Retry-After as a response header, not a body member.
	{Slug: RateLimited, Title: "Rate limited", Status: 429, Exit: 3},
	{Slug: InternalError, Title: "Internal error", Status: 500, Exit: 1},
	{Slug: CapabilityUnavailable, Title: "Capability unavailable", Status: 503, Exit: 4, Extensions: []string{"provider", "reason"}},
	{Slug: ComposePluginMissing, Title: "Compose plugin missing", Status: 503, Exit: 4, Extensions: []string{"provider"}},
	// A failed container update also carries rolledBack and its digests (WI 6.4, FR-13).
	{Slug: DependencyUnavailable, Title: "Dependency unavailable", Status: 503, Exit: 1,
		Extensions: []string{"provider", "rolledBack", "previousDigest", "newDigest"}},
	{Slug: AuditUnavailable, Title: "Audit unavailable", Status: 503, Exit: 1},
	{Slug: StorageLimitReached, Title: "Storage limit reached", Status: 507, Exit: 1},
	// nexusd self-management. config-invalid's errors[] holds ConfigError
	// objects rather than validation-failed's FieldErrors.
	{Slug: ConfigInvalid, Title: "Configuration invalid", Status: 422, Exit: 1, Extensions: []string{"configPath", "errors"}},
	{Slug: UploadTooLarge, Title: "Upload too large", Status: 413, Exit: 2, Extensions: []string{"maxBytes"}},
	{Slug: UploadNotFound, Title: "Upload not found", Status: 422, Exit: 1, Extensions: []string{"uploadId"}},
	{Slug: UpdateNotConfigured, Title: "Update not configured", Status: 409, Exit: 1},
	{Slug: UpdateSignatureInvalid, Title: "Update signature invalid", Status: 422, Exit: 1},
	{Slug: UpdatePackageInvalid, Title: "Update package invalid", Status: 422, Exit: 1, Extensions: []string{"check"}},
	{Slug: VersionDowngrade, Title: "Version downgrade", Status: 409, Exit: 1, Extensions: []string{"runningVersion", "packageVersion"}},
	{Slug: RollbackUnavailable, Title: "Rollback unavailable", Status: 409, Exit: 1, Extensions: []string{"path"}},
	// pihole-dns-zones.
	// Like dependency-unavailable, dns-file-conflict and dns-batch-rejected
	// may also appear in operation.error: the DNS write pipeline re-checks
	// them after admission, inside the operation.
	{Slug: DNSFileConflict, Title: "DNS file conflict", Status: 409, Exit: 1, Extensions: []string{"expectedEtag", "currentEtag"}},
	{Slug: DNSNameUnzoned, Title: "DNS name not in a zone", Status: 422, Exit: 1, Extensions: []string{"name"}},
	{Slug: DNSBatchRejected, Title: "DNS batch rejected", Status: 422, Exit: 1, Extensions: []string{"changeIndex", "cause"}},

	// An operation that fails makes the CLI exit 1.
	{Slug: OutcomeUnknown, Title: "Outcome unknown", Exit: 1, OperationLevel: true},
	{Slug: PackageManagerBusy, Title: "Package manager busy", Exit: 1, OperationLevel: true},
	{Slug: PackageManagerBroken, Title: "Package manager broken", Exit: 1, OperationLevel: true},
	// A failed apt run still reports what it changed (CR-24, FR-15): the
	// outcome members are those of the result a successful run returns.
	{Slug: AptFailed, Title: "Apt failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"changes", "conffilesKept", "conffilesComplete", "rebootRequired",
			"rebootRequiredPackages", "exitCode"}},
	// A script's outcome carries how it ended (WI 5.3).
	{Slug: ScriptFailed, Title: "Script failed", Exit: 1, OperationLevel: true, Extensions: []string{"exitCode", "signal", "durationSeconds"}},
	{Slug: ScriptTimeout, Title: "Script timed out", Exit: 1, OperationLevel: true, Extensions: []string{"exitCode", "signal", "durationSeconds"}},
	{Slug: UpdateHealthFailed, Title: "Update health check failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"rolledBack", "previousDigest", "newDigest"}},
	// observed is a ReloadObservation; rollbackReload is absent when nginx
	// was no longer running to reload after the restore.
	{Slug: NginxReloadFailed, Title: "nginx reload failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"observed", "rolledBack", "rollbackReload", "testOutput"}},
	// nexusd self-management: the outcomes of reload, restart, update and
	// rollback operations.
	{Slug: ConfigApplyFailed, Title: "Configuration apply failed", Exit: 1, OperationLevel: true},
	{Slug: RestartUnhealthy, Title: "Restart unhealthy", Exit: 1, OperationLevel: true, Extensions: []string{"condition", "version"}},
	{Slug: UpdateFailed, Title: "Update failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"rolledBack", "step", "previousVersion", "newVersion"}},
	{Slug: UpdateRollbackFailed, Title: "Update rollback failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"rolledBack", "step", "previousVersion", "newVersion"}},
	{Slug: RollbackFailed, Title: "Rollback failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"rolledBack", "condition", "fromVersion", "toVersion"}},
	// pihole-dns-zones: the outcomes of DNS writes, gravity and pihole commands.
	{Slug: DNSConfigTestFailed, Title: "DNS configuration test failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"exitCode", "rolledBack", "preExisting"}},
	{Slug: DNSVerificationFailed, Title: "DNS verification failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"rolledBack", "name", "expected", "observed"}},
	// The rollback restart itself failed, so DNS may be down.
	{Slug: DNSRollbackFailed, Title: "DNS rollback failed", Exit: 1, OperationLevel: true, Extensions: []string{"rollbackCopy"}},
	{Slug: GravityFailed, Title: "Gravity update failed", Exit: 1, OperationLevel: true, Extensions: []string{"exitCode"}},
	{Slug: PiholeCommandFailed, Title: "Pi-hole command failed", Exit: 1, OperationLevel: true,
		Extensions: []string{"command", "exitCode", "rolledBack"}},
	{Slug: PiholeBusy, Title: "Pi-hole busy", Exit: 1, OperationLevel: true, Extensions: []string{"waitedSeconds"}},
}

var bySlug = func() map[Slug]Type {
	m := make(map[Slug]Type, len(registry))
	for _, t := range registry {
		if _, dup := m[t.Slug]; dup {
			panic("problem: duplicate slug " + string(t.Slug))
		}
		m[t.Slug] = t
	}
	return m
}()

// Lookup returns the registered type for slug.
func Lookup(slug Slug) (Type, bool) {
	t, ok := bySlug[slug]
	return t, ok
}

// LookupURI returns the registered type for a type URI.
func LookupURI(uri string) (Type, bool) {
	slug, ok := strings.CutPrefix(uri, TypePrefix)
	if !ok {
		return Type{}, false
	}
	return Lookup(Slug(slug))
}

// All returns every registered type in table order.
func All() []Type { return cloneTypes(registry) }

// HTTP returns the HTTP-level types.
func HTTP() []Type {
	return slices.DeleteFunc(All(), func(t Type) bool { return t.OperationLevel })
}

// OperationLevel returns the types that appear only inside operation.error.
func OperationLevel() []Type {
	return slices.DeleteFunc(All(), func(t Type) bool { return !t.OperationLevel })
}

func cloneTypes(ts []Type) []Type {
	out := make([]Type, len(ts))
	for i, t := range ts {
		t.Extensions = slices.Clone(t.Extensions)
		out[i] = t
	}
	return out
}
