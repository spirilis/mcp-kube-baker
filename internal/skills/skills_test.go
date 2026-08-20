// Package skills_test is deliberately an external test package. internal/tools
// imports internal/skills in production; only an external test package may
// import back the other way, which these tests need in order to check the
// shipped prose against the real tool and template registries.
package skills_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-kube-baker/internal/resources"
	"github.com/spirilis/mcp-kube-baker/internal/skills"
	"github.com/spirilis/mcp-kube-baker/internal/tools"
)

// wantAlwaysOn is every skill served without enabling anything. Spelled out so
// adding or removing one is a deliberate edit rather than a silent drift.
var wantAlwaysOn = []string{"skill://pod-triage/SKILL.md"}

func TestRegisterAllLoadsExpectedSkills(t *testing.T) {
	rr := mcp.NewResourceRegistry()
	sr, err := skills.RegisterAll(rr)
	if err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	got := make([]string, 0, len(sr.List()))
	for _, s := range sr.List() {
		got = append(got, s.URI)
		// The library enforces frontmatter name == directory name, so reaching
		// here at all proves that. What it does not enforce is a description
		// worth reading, and description is the only thing a host sees before
		// deciding whether to fetch the body.
		desc, _ := s.Frontmatter["description"].(string)
		if len(desc) < 40 {
			t.Errorf("%s: description is too thin to route on: %q", s.URI, desc)
		}
	}
	slices.Sort(got)
	want := slices.Clone(wantAlwaysOn)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("always-on skills = %v, want %v", got, want)
	}
}

// TestSkillDigestsMatchResourcesRead checks the one invariant the extension
// rests on: the digest published in skills/list is over exactly the bytes
// resources/read returns for that URI. A host that verifies digests — which
// SEP-2640 says it MUST — rejects the skill outright if this drifts.
func TestSkillDigestsMatchResourcesRead(t *testing.T) {
	rr := mcp.NewResourceRegistry()
	sr, err := skills.RegisterAll(rr)
	if err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	ctx := context.Background()
	for _, s := range sr.List() {
		if len(s.Resources) == 0 {
			t.Errorf("%s publishes no resources", s.URI)
		}
		for _, res := range s.Resources {
			content, err := rr.Read(ctx, res.URI)
			if err != nil {
				t.Errorf("%s: resources/read: %v", res.URI, err)
				continue
			}
			raw := []byte(content.Text)
			if content.Blob != "" {
				if raw, err = base64.StdEncoding.DecodeString(content.Blob); err != nil {
					t.Errorf("%s: decoding blob: %v", res.URI, err)
					continue
				}
			}
			if got := fmt.Sprintf("sha256:%x", sha256.Sum256(raw)); got != res.Digest {
				t.Errorf("%s: published digest %s, but resources/read hashes to %s", res.URI, res.Digest, got)
			}
		}
	}
}

// TestPluginSkillsAreValid runs the same validation newPluginManager does, so a
// malformed plugin skill fails here rather than at a client's first enable call.
func TestPluginSkillsAreValid(t *testing.T) {
	for _, p := range tools.Catalog() {
		fsys := p.Surface(nil).Skills
		if fsys == nil {
			continue
		}
		t.Run(p.Name, func(t *testing.T) {
			uris, err := skills.URIsIn(fsys)
			if err != nil {
				t.Fatalf("skills do not load: %v", err)
			}
			if len(uris) == 0 {
				t.Error("a non-nil Skills filesystem should contain at least one skill")
			}
		})
	}
}

// TestSkillURIsAreUniqueAcrossTiers guards a footgun the library cannot catch:
// registering a skill under a URI another skill already holds is treated as
// replacement, not conflict. So two plugins — or a plugin and the always-on set
// — sharing a skill directory name would silently shadow each other, and
// disabling one would withdraw the other's content.
func TestSkillURIsAreUniqueAcrossTiers(t *testing.T) {
	owners := map[string]string{}
	claim := func(t *testing.T, owner string, uris []string) {
		for _, uri := range uris {
			if prev, dup := owners[uri]; dup {
				t.Errorf("%s is published by both %s and %s; skill directory names must be unique", uri, prev, owner)
				continue
			}
			owners[uri] = owner
		}
	}

	content, err := skills.ContentFS()
	if err != nil {
		t.Fatalf("ContentFS: %v", err)
	}
	alwaysOn, err := skills.URIsIn(content)
	if err != nil {
		t.Fatalf("URIsIn(always-on): %v", err)
	}
	claim(t, "the always-on set", alwaysOn)

	for _, p := range tools.Catalog() {
		fsys := p.Surface(nil).Skills
		if fsys == nil {
			continue
		}
		uris, err := skills.URIsIn(fsys)
		if err != nil {
			t.Fatalf("URIsIn(%s): %v", p.Name, err)
		}
		claim(t, "plugin "+p.Name, uris)
	}
}

var (
	toolRef = regexp.MustCompile(`\bkubectl_[a-z0-9_]+\b`)
	uriRef  = regexp.MustCompile(`mcp\+kubectl://\S+`)
)

// TestSkillContentReferencesRealToolsAndResources is the doc-rot guard, and the
// reason this file exists.
//
// A skill's whole value is that it names this server's actual tools, arguments
// and resource URIs instead of giving generic kubectl advice. That value is
// entirely contingent on those names still being real, and nothing else in the
// build would notice a rename: prose does not compile. So every kubectl_*
// identifier and every mcp+kubectl:// URI in every shipped skill — always-on and
// plugin-scoped alike — is checked against the live registries here.
//
// The URI half works because the prose writes URIs in the same {context}
// placeholder style the tool descriptions use: MatchTemplate matches
// structurally on segment count and literal segments, so the literal text
// {context} binds to the {context} variable like any other single-segment value.
func TestSkillContentReferencesRealToolsAndResources(t *testing.T) {
	// tools.RegisterAll, resources.RegisterAll and Plugin.Surface only close
	// over their kube.Clients argument — none of them calls a method on it at
	// registration time — so a nil client source is enough here.
	tr := mcp.NewToolRegistry()
	tools.RegisterAll(tr, nil)
	known := map[string]bool{}
	for _, def := range tr.List() {
		known[def.Name] = true
	}
	// Registered by RegisterPlugins rather than RegisterAll, and generated from
	// the catalog, so they are not in either loop.
	known["kubectl_plugin_enable"] = true
	known["kubectl_plugin_disable"] = true
	for _, p := range tools.Catalog() {
		for _, pt := range p.Surface(nil).Tools {
			known[pt.Def.Name] = true
		}
	}

	rr := mcp.NewResourceRegistry()
	if err := resources.RegisterAll(rr, nil); err != nil {
		t.Fatalf("resources.RegisterAll: %v", err)
	}

	for _, tier := range skillTiers(t) {
		t.Run(tier.name, func(t *testing.T) {
			err := fs.WalkDir(tier.fsys, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				b, err := fs.ReadFile(tier.fsys, p)
				if err != nil {
					return err
				}
				body := string(b)
				for _, ref := range toolRef.FindAllString(body, -1) {
					if !known[ref] {
						t.Errorf("%s: names tool %q, which this build does not register", p, ref)
					}
				}
				for _, ref := range uriRef.FindAllString(body, -1) {
					uri := strings.TrimRight(ref, ".,;:)]`'\"")
					if _, _, _, ok := rr.MatchTemplate(uri); !ok {
						t.Errorf("%s: names %q, which matches no registered resource template", p, uri)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walking skill content: %v", err)
			}
		})
	}
}

type skillTier struct {
	name string
	fsys fs.FS
}

// skillTiers is every skill filesystem this build ships, so a check written
// once covers the always-on set and every plugin's content alike.
func skillTiers(t *testing.T) []skillTier {
	t.Helper()
	content, err := skills.ContentFS()
	if err != nil {
		t.Fatalf("ContentFS: %v", err)
	}
	tiers := []skillTier{{name: "always-on", fsys: content}}
	for _, p := range tools.Catalog() {
		if fsys := p.Surface(nil).Skills; fsys != nil {
			tiers = append(tiers, skillTier{name: "plugin-" + p.Name, fsys: fsys})
		}
	}
	return tiers
}
