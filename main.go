// Command immich-transcode re-encodes large, inefficient Immich videos to HEVC.
package main

import (
	"os"

	"immich-transcode/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
