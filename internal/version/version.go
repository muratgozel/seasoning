// Package version holds the build version, stamped by -ldflags at release.
package version

// Version is "dev" for local builds; releases set it to the semver without "v".
var Version = "dev"
