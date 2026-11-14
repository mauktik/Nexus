package def

import "slices"

// approvedVerbs is the CLI verb list from FR-29, modeled on PowerShell's
// approved verbs, plus reload, rollback and validate-config for daemon
// self-management (nexusd-self-management FR-13). There are deliberately no
// synonyms: listing is "get", not "list"; deletion is "remove", not "delete";
// running a script is "invoke".
var approvedVerbs = []string{
	"get",
	"start",
	"stop",
	"restart",
	"update",
	"install",
	"remove",
	"invoke",
	"test",
	"add",
	"cancel",
	"reload",
	"rollback",
	"validate-config",
}

// ApprovedVerbs returns a copy of the approved CLI verbs.
func ApprovedVerbs() []string { return slices.Clone(approvedVerbs) }

// IsApprovedVerb reports whether v is an approved CLI verb. Matching is exact.
func IsApprovedVerb(v string) bool { return slices.Contains(approvedVerbs, v) }
