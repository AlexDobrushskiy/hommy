// Package vaulttemplate embeds the hommy vault template (master spec §6): the files of this directory, which
// `hommy vault init` writes into an empty folder. This file is not part of the template and is never copied.
package vaulttemplate

import "embed"

// FS holds the template tree, rooted at this directory.
//
//go:embed AGENTS.md CLAUDE.md index.md log.md context active decisions inbox archive prompt all:_templates
var FS embed.FS
