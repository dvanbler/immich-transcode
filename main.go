// Command immich-transcode re-encodes large, inefficient Immich videos to HEVC.
package main

import (
	"os"

	"immich-transcode/internal/cli"
)

// version is set at build time: go build -ldflags "-X main.version=v0.1.0".
// The Makefile fills it in from `git describe`.
var version string

func main() {
	os.Exit(cli.Execute(version))
}
