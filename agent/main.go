package main

import (
	"os"

	"mooncell/agent/internal/agentapp"
)

// agentVersion 可在构建时用 -ldflags "-X main.agentVersion=vX.Y.Z" 覆盖(发布打版用)。
var agentVersion = "v0.1.0"

func main() {
	agentapp.Run(agentVersion, os.Args[1:])
}
