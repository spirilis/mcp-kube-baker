package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

const (
	surfacePluginTool     = "test_surface_tool"
	surfacePluginResource = "mcp+test://thing"
	surfacePluginTemplate = "mcp+test://thing/{id}"
)

// surfacePlugin carries one of each kind of contribution. The shipped karpenter
// plugin contributes tools only, so without this the resource and template
// halves of enable/disable would ship untested.
func surfacePlugin() Plugin {
	return Plugin{
		Name:    "surface",
		Summary: "Test-only plugin carrying one tool, one resource and one template.",
		Surface: func(kube.Clients) PluginSurface {
			return PluginSurface{
				Tools: []PluginTool{{
					Def: mcp.Tool{Name: surfacePluginTool, InputSchema: json.RawMessage(`{"type":"object"}`)},
					Fn: func(context.Context, *mcp.ToolRequest) (mcp.Result, error) {
						return &mcp.ToolCallResult{Content: []mcp.Content{mcp.Text("ok")}}, nil
					},
				}},
				Resources: []PluginResource{{
					Def: mcp.Resource{URI: surfacePluginResource, Name: "Thing", MimeType: "text/plain"},
					Fn: func(context.Context) (mcp.ResourceContentResult, error) {
						return mcp.ResourceContentResult{Text: "thing"}, nil
					},
				}},
				Templates: []PluginTemplate{{
					Def: mcp.ResourceTemplate{URITemplate: surfacePluginTemplate, Name: "Thing by id", MimeType: "text/plain"},
					Fn: func(context.Context, *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
						return mcp.ResourceContentResult{Text: "thing"}, nil
					},
				}},
			}
		},
	}
}

const (
	skillPluginSkillURI = "skill://test-surface/SKILL.md"
	skillPluginFileURI  = "skill://test-surface/references/more.md"
)

// skillPlugin carries a skill as well as the rest of a surface, so the
// PluginSurface.Skills half of enable/disable is exercised without depending on
// the shipped karpenter content — which can change for reasons of its own.
func skillPlugin() Plugin {
	p := surfacePlugin()
	p.Name = "skilled"
	inner := p.Surface
	p.Surface = func(c kube.Clients) PluginSurface {
		s := inner(c)
		s.Skills = fstest.MapFS{
			"test-surface/SKILL.md": &fstest.MapFile{Data: []byte(
				"---\nname: test-surface\ndescription: Test-only skill.\n---\n\nBody.\n")},
			"test-surface/references/more.md": &fstest.MapFile{Data: []byte("More.\n")},
		}
		return s
	}
	return p
}

// malformedSkillPlugin ships a SKILL.md whose frontmatter name does not match
// its directory, which the library rejects.
func malformedSkillPlugin() Plugin {
	p := skillPlugin()
	p.Name = "malformed"
	inner := p.Surface
	p.Surface = func(c kube.Clients) PluginSurface {
		s := inner(c)
		s.Skills = fstest.MapFS{
			"test-surface/SKILL.md": &fstest.MapFile{Data: []byte(
				"---\nname: something-else\ndescription: Mismatched.\n---\n")},
		}
		return s
	}
	return p
}

// brokenPlugin registers a tool and then a URI template that cannot compile, so
// the rollback path has something to roll back.
func brokenPlugin() Plugin {
	p := surfacePlugin()
	p.Name = "broken"
	inner := p.Surface
	p.Surface = func(c kube.Clients) PluginSurface {
		s := inner(c)
		s.Templates[0].Def.URITemplate = "mcp+test://broken/{unterminated"
		return s
	}
	return p
}

// newTestPluginManager builds a manager over plugins, with fresh registries the
// caller can inspect directly — asserting against the registry rather than the
// manager's own bookkeeping is the whole point of most of these tests.
func newTestPluginManager(t *testing.T, plugins ...Plugin) (*PluginManager, *mcp.ToolRegistry, *mcp.ResourceRegistry, *mcp.SkillRegistry) {
	t.Helper()
	tr := mcp.NewToolRegistry()
	rr := mcp.NewResourceRegistry()
	sr := mcp.NewSkillRegistry(rr)
	m, err := newPluginManager(plugins, tr, rr, sr, newFakeClients())
	if err != nil {
		t.Fatalf("newPluginManager: %v", err)
	}
	return m, tr, rr, sr
}

// callPluginAction runs one meta-tool handler and decodes its output.
func callPluginAction(t *testing.T, fn mcp.ToolFunction, args string) (*mcp.ToolCallResult, pluginActionOutput) {
	t.Helper()
	res := callTool(t, fn, args)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(t, res))
	}
	out, ok := res.StructuredContent.(pluginActionOutput)
	if !ok {
		t.Fatalf("expected pluginActionOutput, got %T", res.StructuredContent)
	}
	return res, out
}

func TestCatalogContainsKarpenter(t *testing.T) {
	for _, p := range Catalog() {
		if p.Name != "karpenter" {
			continue
		}
		names := surfaceToolNames(p.Surface(newFakeClients()))
		if !slices.Contains(names, "kubectl_karpenter_logs") {
			t.Fatalf("the karpenter plugin should provide kubectl_karpenter_logs, got %v", names)
		}
		return
	}
	t.Fatal("the catalog has no karpenter plugin")
}

// TestCatalogSurfacesRegisterCleanly is what keeps enable's rollback path
// unreachable in practice: a URI template that cannot compile is a defect in the
// plugin, and this is where it gets caught.
func TestCatalogSurfacesRegisterCleanly(t *testing.T) {
	for _, p := range Catalog() {
		m, _, _, _ := newTestPluginManager(t, p)
		if _, err := m.enable(p.Name); err != nil {
			t.Errorf("plugin %q does not register cleanly: %v", p.Name, err)
		}
	}
}

func TestPluginEnableRegistersTools(t *testing.T) {
	m, tr, _, _ := newTestPluginManager(t, Catalog()...)

	if _, ok := tr.Get("kubectl_karpenter_logs"); ok {
		t.Fatal("a plugin's tools must not be registered before it is enabled")
	}

	_, out := callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"karpenter"}`)
	if out.Action != "enabled" {
		t.Errorf("action = %q, want enabled", out.Action)
	}
	// Against the registry, not the manager: this is what proves the live tool
	// list actually changed, which is what the client will see.
	if _, ok := tr.Get("kubectl_karpenter_logs"); !ok {
		t.Error("enabling karpenter should have registered kubectl_karpenter_logs")
	}
	if !pluginRow(t, out, "karpenter").Enabled {
		t.Error("the result should report karpenter as enabled")
	}
}

func TestPluginDisableUnregistersTools(t *testing.T) {
	m, tr, _, _ := newTestPluginManager(t, Catalog()...)
	callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"karpenter"}`)

	_, out := callPluginAction(t, NewPluginDisableHandler(m), `{"plugin_name":"karpenter"}`)
	if out.Action != "disabled" {
		t.Errorf("action = %q, want disabled", out.Action)
	}
	if _, ok := tr.Get("kubectl_karpenter_logs"); ok {
		t.Error("disabling karpenter should have unregistered kubectl_karpenter_logs")
	}
	if pluginRow(t, out, "karpenter").Enabled {
		t.Error("the result should report karpenter as disabled")
	}
}

func TestPluginEnableRegistersResourcesAndTemplates(t *testing.T) {
	m, tr, rr, _ := newTestPluginManager(t, surfacePlugin())

	if _, err := m.enable("surface"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, ok := tr.Get(surfacePluginTool); !ok {
		t.Error("the plugin's tool should be registered")
	}
	if _, ok := rr.Get(surfacePluginResource); !ok {
		t.Error("the plugin's concrete resource should be registered")
	}
	if !slices.Contains(templateURIs(rr), surfacePluginTemplate) {
		t.Errorf("the plugin's template should be registered, got %v", templateURIs(rr))
	}
	// A template that is listed but does not match is a template that will never
	// serve anything, so check the matcher and not just the catalog.
	if _, _, _, ok := rr.MatchTemplate("mcp+test://thing/abc"); !ok {
		t.Error("the registered template should match a concrete URI")
	}
}

func TestPluginDisableUnregistersResourcesAndTemplates(t *testing.T) {
	m, tr, rr, _ := newTestPluginManager(t, surfacePlugin())
	if _, err := m.enable("surface"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	if _, err := m.disable("surface"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := tr.Get(surfacePluginTool); ok {
		t.Error("the plugin's tool should be gone")
	}
	if _, ok := rr.Get(surfacePluginResource); ok {
		t.Error("the plugin's concrete resource should be gone")
	}
	if slices.Contains(templateURIs(rr), surfacePluginTemplate) {
		t.Errorf("the plugin's template should be gone, got %v", templateURIs(rr))
	}
	if _, _, _, ok := rr.MatchTemplate("mcp+test://thing/abc"); ok {
		t.Error("a disabled plugin's template must no longer match")
	}
}

// TestPluginEnablePublishesSkills is the skills counterpart of the tools and
// resources cases: a plugin's written procedures appear exactly when its tools
// do. Checked against the registries rather than the manager, since that is what
// answers skills/list and resources/read.
func TestPluginEnablePublishesSkills(t *testing.T) {
	m, _, rr, sr := newTestPluginManager(t, skillPlugin())

	if sr.Has() {
		t.Fatal("a plugin's skills must not be published before it is enabled")
	}

	_, out := callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"skilled"}`)

	if _, ok := sr.Get(skillPluginSkillURI); !ok {
		t.Errorf("enabling should have published %s, got %v", skillPluginSkillURI, skillURIs(sr))
	}
	// A skill's files are ordinary resources; if they are not readable then
	// skills/list is advertising digests for content resources/read cannot
	// serve, which is the one invariant the extension rests on.
	for _, uri := range []string{skillPluginSkillURI, skillPluginFileURI} {
		if _, ok := rr.Get(uri); !ok {
			t.Errorf("skill file %s should be a readable resource", uri)
		}
	}
	if got := pluginRow(t, out, "skilled").Skills; !slices.Contains(got, skillPluginSkillURI) {
		t.Errorf("the result should name the published skill, got %v", got)
	}
}

func TestPluginDisableWithdrawsSkills(t *testing.T) {
	m, _, rr, sr := newTestPluginManager(t, skillPlugin())
	callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"skilled"}`)

	callPluginAction(t, NewPluginDisableHandler(m), `{"plugin_name":"skilled"}`)

	if _, ok := sr.Get(skillPluginSkillURI); ok {
		t.Errorf("disabling should have withdrawn %s", skillPluginSkillURI)
	}
	for _, uri := range []string{skillPluginSkillURI, skillPluginFileURI} {
		if _, ok := rr.Get(uri); ok {
			t.Errorf("skill file %s should be gone from the resource registry", uri)
		}
	}
}

// TestPluginSkillsAbsentWhenServerServesNone is the skills.enabled: false case.
// Turning skills off must not turn plugins off — only their prose goes away.
func TestPluginSkillsAbsentWhenServerServesNone(t *testing.T) {
	tr := mcp.NewToolRegistry()
	rr := mcp.NewResourceRegistry()
	m, err := newPluginManager([]Plugin{skillPlugin()}, tr, rr, nil, newFakeClients())
	if err != nil {
		t.Fatalf("newPluginManager with no skill registry: %v", err)
	}

	_, out := callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"skilled"}`)

	if _, ok := tr.Get(surfacePluginTool); !ok {
		t.Error("the plugin's tool should still be registered when skills are off")
	}
	for _, res := range rr.List() {
		if strings.HasPrefix(res.URI, "skill://") {
			t.Errorf("no skill resource should be registered, found %s", res.URI)
		}
	}
	// Not merely unpublished: never advertised either, so a model is not told
	// about prose it cannot fetch.
	if got := pluginRow(t, out, "skilled").Skills; len(got) != 0 {
		t.Errorf("the result should advertise no skills, got %v", got)
	}
	if desc := PluginEnableDefinition(m).Description; strings.Contains(desc, "Adds skills:") {
		t.Error("the catalog description should not advertise skills the server will not serve")
	}
}

// TestPluginWithMalformedSkillIsRejectedAtConstruction is why newPluginManager
// returns an error. Embedded content can only be wrong by build defect, so the
// failure belongs at startup — not at the first client that happens to enable
// the plugin.
func TestPluginWithMalformedSkillIsRejectedAtConstruction(t *testing.T) {
	_, err := newPluginManager([]Plugin{malformedSkillPlugin()},
		mcp.NewToolRegistry(), mcp.NewResourceRegistry(),
		mcp.NewSkillRegistry(mcp.NewResourceRegistry()), newFakeClients())
	if err == nil {
		t.Fatal("a plugin whose skill frontmatter does not match its directory should be rejected")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Errorf("the error should name the offending plugin, got %v", err)
	}
}

// TestKarpenterPluginPublishesItsSkill pins the shipped content, not just the
// mechanism: the karpenter procedure is the reason PluginSurface.Skills exists.
func TestKarpenterPluginPublishesItsSkill(t *testing.T) {
	const want = "skill://karpenter-nodes/SKILL.md"
	m, _, _, sr := newTestPluginManager(t, Catalog()...)

	if _, ok := sr.Get(want); ok {
		t.Fatal("the karpenter skill must not be published before the plugin is enabled")
	}
	if _, err := m.enable("karpenter"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, ok := sr.Get(want); !ok {
		t.Errorf("enabling karpenter should publish %s, got %v", want, skillURIs(sr))
	}
	if _, err := m.disable("karpenter"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := sr.Get(want); ok {
		t.Errorf("disabling karpenter should withdraw %s", want)
	}
}

// TestPluginEnableRollsBackOnTemplateError covers the one way enable can fail
// after it has already changed a registry: a plugin must never be left with some
// of its surface registered.
func TestPluginEnableRollsBackOnTemplateError(t *testing.T) {
	m, tr, rr, _ := newTestPluginManager(t, brokenPlugin())

	if _, err := m.enable("broken"); err == nil {
		t.Fatal("a malformed URI template should make enable fail")
	}
	if _, ok := tr.Get(surfacePluginTool); ok {
		t.Error("the tool registered before the bad template should have been rolled back")
	}
	if _, ok := rr.Get(surfacePluginResource); ok {
		t.Error("the resource registered before the bad template should have been rolled back")
	}
	// Still disabled, so a later disable is a no-op rather than a double removal.
	if _, out := callPluginAction(t, NewPluginDisableHandler(m), `{"plugin_name":"broken"}`); out.Action != "already_disabled" {
		t.Errorf("a plugin that failed to enable must still be disabled, got action %q", out.Action)
	}
}

// TestPluginEnableIdempotentDoesNotReRegister leans on a registry detail to
// observe something otherwise invisible: Register on an existing name moves it
// to the end of the list. If the tool has not moved, the real Register was
// skipped — and so was the spurious list_changed it would have sent.
func TestPluginEnableIdempotentDoesNotReRegister(t *testing.T) {
	m, tr, _, _ := newTestPluginManager(t, Catalog()...)
	callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"karpenter"}`)

	tr.Register(mcp.Tool{Name: "zz_sentinel", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.ToolRequest) (mcp.Result, error) { return &mcp.ToolCallResult{}, nil })
	before := toolNames(tr)

	_, out := callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"karpenter"}`)
	if out.Action != "already_enabled" {
		t.Errorf("action = %q, want already_enabled", out.Action)
	}
	if after := toolNames(tr); !slices.Equal(before, after) {
		t.Errorf("re-enabling reordered the tool list, so it re-registered:\n before %v\n after  %v", before, after)
	}
}

func TestPluginDisableIdempotent(t *testing.T) {
	m, tr, _, _ := newTestPluginManager(t, Catalog()...)
	before := toolNames(tr)

	_, out := callPluginAction(t, NewPluginDisableHandler(m), `{"plugin_name":"karpenter"}`)
	if out.Action != "already_disabled" {
		t.Errorf("action = %q, want already_disabled", out.Action)
	}
	if after := toolNames(tr); !slices.Equal(before, after) {
		t.Errorf("disabling an already-disabled plugin touched the registry:\n before %v\n after  %v", before, after)
	}
}

func TestPluginUnknownNameIsToolError(t *testing.T) {
	m, _, _, _ := newTestPluginManager(t, Catalog()...)

	for name, fn := range map[string]mcp.ToolFunction{
		"enable":  NewPluginEnableHandler(m),
		"disable": NewPluginDisableHandler(m),
	} {
		t.Run(name, func(t *testing.T) {
			res := callTool(t, fn, `{"plugin_name":"nope"}`)
			if !res.IsError {
				t.Fatal("an unknown plugin should be a tool error")
			}
			// The one thing a model can act on is the list of real names.
			if !strings.Contains(contentText(t, res), "karpenter") {
				t.Errorf("the error should name the valid plugins: %s", contentText(t, res))
			}
		})
	}
}

func TestPluginMissingNameIsToolError(t *testing.T) {
	m, _, _, _ := newTestPluginManager(t, Catalog()...)
	res := callTool(t, NewPluginEnableHandler(m), `{}`)
	if !res.IsError {
		t.Fatal("an omitted plugin_name should be a tool error")
	}
	if !strings.Contains(contentText(t, res), "karpenter") {
		t.Errorf("the error should name the valid plugins: %s", contentText(t, res))
	}
}

func TestPluginUnknownNameIsUnknownPluginError(t *testing.T) {
	m, _, _, _ := newTestPluginManager(t, Catalog()...)
	if _, err := m.enable("nope"); !errors.Is(err, ErrUnknownPlugin) {
		t.Errorf("enable of an unknown name should wrap ErrUnknownPlugin, got %v", err)
	}
	if _, err := m.disable("nope"); !errors.Is(err, ErrUnknownPlugin) {
		t.Errorf("disable of an unknown name should wrap ErrUnknownPlugin, got %v", err)
	}
}

// TestPluginActionReturnsFullCatalogStatus is why there is no separate list
// tool: current state rides along on every action.
func TestPluginActionReturnsFullCatalogStatus(t *testing.T) {
	m, _, _, _ := newTestPluginManager(t, append(Catalog(), surfacePlugin())...)
	_, out := callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"karpenter"}`)

	if len(out.Plugins) != len(Catalog())+1 {
		t.Fatalf("the result should report every plugin, got %d of %d", len(out.Plugins), len(Catalog())+1)
	}
	if !pluginRow(t, out, "karpenter").Enabled {
		t.Error("karpenter should be reported enabled")
	}
	row := pluginRow(t, out, "surface")
	if row.Enabled {
		t.Error("an untouched plugin should be reported disabled")
	}
	// Contributions are reported whether or not the plugin is on, so a model can
	// tell what enabling would cost before it does.
	if !slices.Contains(row.Tools, surfacePluginTool) ||
		!slices.Contains(row.Resources, surfacePluginResource) ||
		!slices.Contains(row.Templates, surfacePluginTemplate) {
		t.Errorf("a disabled plugin's contributions should still be listed: %+v", row)
	}
}

func TestPluginDescriptionsListCatalog(t *testing.T) {
	m, _, _, _ := newTestPluginManager(t, append(Catalog(), surfacePlugin())...)

	for name, def := range map[string]mcp.Tool{
		"enable":  PluginEnableDefinition(m),
		"disable": PluginDisableDefinition(m),
	} {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				"karpenter", "kubectl_karpenter_logs",
				// A plugin's skills are advertised alongside its tools, so a
				// model can weigh what enabling actually brings.
				"Adds skills: skill://karpenter-nodes/SKILL.md.",
				"surface", surfacePluginTool, surfacePluginResource, surfacePluginTemplate,
			} {
				if !strings.Contains(def.Description, want) {
					t.Errorf("the description should mention %q:\n%s", want, def.Description)
				}
			}

			// The enum is generated from the catalog, so it can never disagree
			// with the description above it.
			var schema struct {
				Properties struct {
					PluginName struct {
						Enum []string `json:"enum"`
					} `json:"plugin_name"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
				t.Fatalf("input schema is not valid JSON: %v", err)
			}
			if !slices.Equal(schema.Properties.PluginName.Enum, m.Names()) {
				t.Errorf("enum = %v, want %v", schema.Properties.PluginName.Enum, m.Names())
			}
		})
	}
}

// TestPluginDescriptionsAreStableAcrossEnableDisable guards the decision not to
// render live state into the descriptions: doing so would mean re-registering
// the meta-tools on every toggle, sending a spurious list_changed and churning
// the tool order clients cache on.
func TestPluginDescriptionsAreStableAcrossEnableDisable(t *testing.T) {
	m, _, _, _ := newTestPluginManager(t, Catalog()...)
	before := PluginEnableDefinition(m).Description

	callPluginAction(t, NewPluginEnableHandler(m), `{"plugin_name":"karpenter"}`)
	callPluginAction(t, NewPluginDisableHandler(m), `{"plugin_name":"karpenter"}`)

	if after := PluginEnableDefinition(m).Description; after != before {
		t.Errorf("the description changed across a toggle:\n before %q\n after  %q", before, after)
	}
}

func TestRegisterPluginsRegistersOnlyMetaTools(t *testing.T) {
	tr := mcp.NewToolRegistry()
	rr := mcp.NewResourceRegistry()
	sr := mcp.NewSkillRegistry(rr)
	if _, err := RegisterPlugins(tr, rr, sr, newFakeClients()); err != nil {
		t.Fatalf("RegisterPlugins: %v", err)
	}

	for _, want := range []string{"kubectl_plugin_enable", "kubectl_plugin_disable"} {
		if _, ok := tr.Get(want); !ok {
			t.Errorf("%s should always be registered", want)
		}
	}
	if _, ok := tr.Get("kubectl_karpenter_logs"); ok {
		t.Error("no plugin should be enabled at startup")
	}
	if len(rr.List()) != 0 || len(rr.ListTemplates()) != 0 {
		t.Error("registering the meta-tools should not touch the resource registry")
	}
	// A plugin's skills are as inert at startup as its tools: nothing is
	// published until a client asks for it.
	if sr.Has() {
		t.Error("registering the meta-tools should not publish any plugin skill")
	}
}

// TestPluginMetaToolsAreNotReadOnly records the one honest exception to this
// server being read-only: these two change nothing on any cluster, but they do
// change which tools it serves.
func TestPluginMetaToolsAreNotReadOnly(t *testing.T) {
	m, _, _, _ := newTestPluginManager(t, Catalog()...)
	for name, def := range map[string]mcp.Tool{
		"enable":  PluginEnableDefinition(m),
		"disable": PluginDisableDefinition(m),
	} {
		if def.Annotations.ReadOnlyHint {
			t.Errorf("%s should not claim to be read-only", name)
		}
		if def.Annotations.DestructiveHint == nil || *def.Annotations.DestructiveHint {
			t.Errorf("%s should be explicitly non-destructive", name)
		}
	}
}

func pluginRow(t *testing.T, out pluginActionOutput, name string) pluginStatusRow {
	t.Helper()
	for _, row := range out.Plugins {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("no status row for plugin %q", name)
	return pluginStatusRow{}
}

func skillURIs(sr *mcp.SkillRegistry) []string {
	list := sr.List()
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.URI
	}
	return out
}

func toolNames(tr *mcp.ToolRegistry) []string {
	list := tr.List()
	out := make([]string, len(list))
	for i, tool := range list {
		out[i] = tool.Name
	}
	return out
}

func templateURIs(rr *mcp.ResourceRegistry) []string {
	list := rr.ListTemplates()
	out := make([]string, len(list))
	for i, tmpl := range list {
		out[i] = tmpl.URITemplate
	}
	return out
}
