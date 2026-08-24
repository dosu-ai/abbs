// Package version reports the abbs build version.
package version

import (
	"runtime/debug"
	"strings"
)

const developmentVersion = "0.0.0-dev"

// Version is set at release time via -ldflags. Development and `go install`
// builds fall back to the module version recorded by the Go toolchain.
var Version = developmentVersion

// Value returns the SemVer without a leading v.
func Value() string {
	info, ok := debug.ReadBuildInfo()
	moduleVersion := ""
	if ok {
		moduleVersion = info.Main.Version
	}
	return resolve(Version, moduleVersion)
}

func resolve(linkerVersion, moduleVersion string) string {
	if linkerVersion != "" && linkerVersion != developmentVersion {
		return strings.TrimPrefix(linkerVersion, "v")
	}
	if moduleVersion != "" && moduleVersion != "(devel)" {
		return strings.TrimPrefix(moduleVersion, "v")
	}
	return developmentVersion
}

// String returns the human-readable version line.
func String() string { return "abbs " + Value() }
