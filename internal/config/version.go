package config

import (
	_ "embed"
	"strings"
)

//go:embed version
var embeddedVersion string

// Version is the release version embedded from the repository version file.
var Version = strings.TrimSpace(embeddedVersion)
