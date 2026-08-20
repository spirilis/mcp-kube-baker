// Package skills serves mcp-kube-baker's embedded Agent Skills: Kubernetes
// troubleshooting procedures written against THIS server's own tool names,
// argument names and mcp+kubectl:// resource URIs. That specificity is the
// point — generic kubectl advice names commands a read-only MCP server cannot
// execute, so the useful thing to ship is the call sequence for these tools.
//
// This package owns the skill:// prefix and every loading operation, so no
// other package re-derives either one. internal/tools reaches for Load and
// URIsIn to give a plugin's skills the same lifetime as its tools.
//
// EXPERIMENTAL: implements the io.modelcontextprotocol/skills extension
// (SEP-2640), an MCP Extensions Track proposal that is open and not merged.
// generic-go-mcp's ServerConfig.Skills makes the whole surface opt-in: a nil
// registry means the three methods answer -32601 and no capability is declared.
package skills

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/spirilis/generic-go-mcp/mcp"
)

// embedded holds every always-on skill directory. Embedding rather than reading
// from disk makes the catalog immutable per build, which is what lets a
// malformed skill be a build-time defect instead of a runtime surprise.
//
//go:embed all:content
var embedded embed.FS

// URIPrefix is the scheme skill files are published under. SEP-2640 makes
// skill:// a SHOULD rather than a MUST, but using it means a skills-aware host
// recognizes these URIs, and keeping skills off mcp+kubectl:// separates static
// documentation from live cluster objects in resources/list.
const URIPrefix = "skill://"

// ContentFS returns the always-on skill tree with the "content/" directory
// stripped, so a skill's URI is skill://<name>/... rather than
// skill://content/<name>/.... Exported so tests walk the exact filesystem
// RegisterAll loads, rather than a second go:embed that could drift from it.
func ContentFS() (fs.FS, error) {
	return fs.Sub(embedded, "content")
}

// Load registers every skill in fsys onto sr. The one place URIPrefix is
// applied, so no caller can publish skills under a different scheme by
// accident.
func Load(sr *mcp.SkillRegistry, fsys fs.FS) error {
	return sr.LoadFS(fsys, URIPrefix)
}

// URIsIn reports the skill URIs that loading fsys would create, by loading it
// into a throwaway registry and reading back what appeared.
//
// Doing it this way rather than recomputing the URIs from the directory names
// buys two things. Malformed content is rejected here — at startup, where it
// belongs, since embedded content can only be wrong by build defect — instead
// of the first time a client enables a plugin. And the URIs come from LoadFS
// itself, so there is no second derivation path to drift from the one that
// actually produced them: whatever escaping or walk rule the library applies,
// these are the strings Unregister will need.
func URIsIn(fsys fs.FS) ([]string, error) {
	scratch := mcp.NewSkillRegistry(mcp.NewResourceRegistry())
	if err := Load(scratch, fsys); err != nil {
		return nil, err
	}
	loaded := scratch.List()
	uris := make([]string, 0, len(loaded))
	for _, s := range loaded {
		uris = append(uris, s.URI)
	}
	return uris, nil
}

// RegisterAll loads the always-on skills onto a new registry backed by rr.
//
// A skill's files are registered as ordinary resources, which is what keeps
// skills/list and resources/read from ever disagreeing about what a skill
// contains — so rr must be the same registry the server is built with.
func RegisterAll(rr *mcp.ResourceRegistry) (*mcp.SkillRegistry, error) {
	content, err := ContentFS()
	if err != nil {
		return nil, fmt.Errorf("opening embedded skill content: %w", err)
	}
	sr := mcp.NewSkillRegistry(rr)
	if err := Load(sr, content); err != nil {
		return nil, fmt.Errorf("loading embedded skills: %w", err)
	}
	return sr, nil
}
