// Package version reports the abbs build version.
package version

// Version is set at release time via -ldflags.
var Version = "0.0.0-dev"

// String returns the human-readable version line.
func String() string { return "abbs " + Version }
