// Package version identifies the running build. Release builds set Version
// with -ldflags "-X github.com/ajaykathwatee/previa/internal/platform/version.Version=v1.2.3".
package version

import "runtime"

// Version is the release, or "dev" for local builds.
var Version = "dev"

// GoVersion is the toolchain the binary was built with.
func GoVersion() string { return runtime.Version() }
