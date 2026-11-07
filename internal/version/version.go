// Package version holds the build version shared by nexusd and nexus.
package version

// Version is the build version. Release builds set it with
// -ldflags "-X github.com/mauktik/nexus/internal/version.Version=<v>".
var Version = "dev"
