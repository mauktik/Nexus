package def

import "slices"

// approvedVerbs is the CLI verb list from FR-29, modeled on PowerShell's
// approved verbs. There are deliberately no synonyms: listing is "get", not
// "list"; deletion is "remove", not "delete"; running a script is "invoke".
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
}

// ApprovedVerbs returns a copy of the approved CLI verbs.
func ApprovedVerbs() []string { return slices.Clone(approvedVerbs) }

// IsApprovedVerb reports whether v is an approved CLI verb. Matching is exact.
func IsApprovedVerb(v string) bool { return slices.Contains(approvedVerbs, v) }
