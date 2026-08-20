package tools

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
	"github.com/spirilis/mcp-kube-baker/internal/skills"
)

// This file implements the plugin system: optional families of tools (and, when
// a plugin wants them, resources and resource templates) that stay out of
// tools/list until a client asks for them by name.
//
// The point is the client's context, not code isolation. Every tool definition
// a server advertises sits in the model's context for the whole session, so a
// server that compiled in every Kubernetes add-on's diagnostics would tax that
// context on clusters running none of them. A plugin is therefore a declarative
// catalog entry, not a compilation unit — plugin tools live in this package
// alongside the core ones and reuse the same helpers.
//
// No notification code is needed here. mcp.ToolRegistry.Register/Unregister and
// their ResourceRegistry counterparts fire the registry's onChange hook, which
// mcp.NewServer wires to the subscription broker; tools/list and
// resources/templates/list read the registries live on every request. Enabling
// a plugin therefore reaches a connected client as
// notifications/tools/list_changed with nothing further from us.
//
// Skills are the one surface where that is not quite enough. A skill's files are
// ordinary resources, so publishing one does fire resources/list_changed — but
// SEP-2640 defines no skills-specific notification, so nothing tells a client
// that skills/list itself changed. Hence pluginStatusRow.Skills: the action
// result names the URIs, which is the only in-band signal available.

// PluginTool is one tool a plugin contributes, already bound to a client source.
type PluginTool struct {
	Def mcp.Tool
	Fn  mcp.ToolFunction
}

// PluginResource is one concrete resource a plugin contributes.
type PluginResource struct {
	Def mcp.Resource
	Fn  mcp.ResourceFunction
}

// PluginTemplate is one resource template a plugin contributes.
//
// Two authoring rules, both inherited from the registry: ResourceRegistry.
// MatchTemplate is first-match-wins in registration order, and a plugin's
// templates are registered after the core ones in internal/resources — so a
// plugin must claim its own path shape (mcp+kubectl://{context}/karpenter/...)
// rather than one a core template already swallows, and within this slice the
// more specific template goes before the more general. Only {var} and a
// terminal {+var} compile.
// A third rule applies to both this and PluginResource: never claim a skill://
// URI. A skill's files are registered as ordinary resources, but mcp's
// SkillRegistry only detects conflicts between skills, so a plugin registering
// skill:// directly would silently shadow a skill file. Contribute skills
// through PluginSurface.Skills instead.
type PluginTemplate struct {
	Def mcp.ResourceTemplate
	Fn  mcp.ResourceTemplateFunction
}

// PluginSurface is everything one plugin adds to the server while enabled.
type PluginSurface struct {
	Tools     []PluginTool
	Resources []PluginResource
	Templates []PluginTemplate
	// Skills is a filesystem whose child directories are skill directories in
	// internal/skills' sense — each one holding a SKILL.md. Registered on
	// enable and unregistered on disable, so a plugin's written procedures have
	// the same lifetime as the tools they describe. Nil for a plugin with none.
	//
	// An fs.FS rather than a slice of mcp.SkillDef so that the library's walk
	// rules, frontmatter parsing and URI escaping stay the only implementation
	// of themselves; see skills.URIsIn for how the resulting URIs are learned.
	Skills fs.FS
}

// pluginSkillFS holds every plugin's skill content, one subdirectory per plugin
// so each can be handed out separately. It lives here, beside the plugins,
// rather than in internal/skills: a plugin's procedures belong to the plugin.
//
//go:embed all:skills
var pluginSkillFS embed.FS

// pluginSkills returns the skill tree for one plugin, rooted so that its child
// directories are the skill directories. A name with no directory yields an FS
// whose walk finds no skills, which newPluginManager reports as the content
// defect it is.
func pluginSkills(plugin string) fs.FS {
	sub, err := fs.Sub(pluginSkillFS, path.Join("skills", plugin))
	if err != nil {
		// fs.Sub rejects only a malformed path, and this one is built from a
		// compile-time constant and a catalog name.
		panic(fmt.Sprintf("plugin skill path for %q: %v", plugin, err))
	}
	return sub
}

// Plugin is one catalog entry. Surface is a constructor rather than a
// materialized value so PluginManager can build each one exactly once, bound to
// one kube.Clients.
type Plugin struct {
	// Name is the plugin_name argument value, e.g. "karpenter".
	Name string
	// Summary is one sentence, rendered into both meta-tools' descriptions.
	Summary string
	Surface func(kube.Clients) PluginSurface
}

// Catalog is every plugin this build ships, in the fixed order that also fixes
// iteration order in the generated descriptions and in every status report.
func Catalog() []Plugin {
	return []Plugin{
		{
			Name:    "karpenter",
			Summary: "Karpenter node autoscaler: find the pod currently holding the leader-election Lease and read its logs.",
			Surface: func(c kube.Clients) PluginSurface {
				return PluginSurface{
					Tools: []PluginTool{
						{Def: KarpenterLogsDefinition(), Fn: NewKarpenterLogsHandler(c)},
					},
					Skills: pluginSkills("karpenter"),
				}
			},
		},
	}
}

// ErrUnknownPlugin names a plugin_name that is not in the catalog.
var ErrUnknownPlugin = errors.New("unknown plugin")

type pluginState struct {
	plugin  Plugin
	surface PluginSurface
	// skillURIs is what loading surface.Skills produces, learned once at
	// construction so disable can undo precisely what enable did. Empty when
	// the plugin ships no skills or the server is not serving them.
	skillURIs []string
	enabled   bool
}

// PluginManager owns every plugin's enabled state and is the only thing that
// mutates either registry after startup.
type PluginManager struct {
	mu        sync.Mutex
	tools     *mcp.ToolRegistry
	resources *mcp.ResourceRegistry
	// skills is nil when the server is not serving skills at all (skills.enabled
	// is false in config). A plugin's tools still come and go in that case; only
	// its written procedures are absent.
	skills *mcp.SkillRegistry
	// plugins is in Catalog() order and is immutable after construction; only
	// each entry's enabled field changes.
	plugins []*pluginState
	byName  map[string]*pluginState
}

// NewPluginManager materializes every catalog entry's surface exactly once,
// bound to c, and validates each one's embedded skills.
//
// Materializing here rather than per Enable call is what keeps enable and
// disable symmetric: disable unregisters precisely the names, URIs, URI-template
// strings and skill URIs enable registered — UnregisterTemplate matches the
// exact template string — and the generated descriptions read their tool names
// from the same values, so there is no second derivation path to drift.
//
// sr may be nil, meaning this server does not serve skills; plugin tools are
// unaffected. An error means a plugin's embedded skill content is malformed,
// which is a build defect rather than anything about the environment — hence
// reported at startup, not at a client's first enable call.
func NewPluginManager(tr *mcp.ToolRegistry, rr *mcp.ResourceRegistry, sr *mcp.SkillRegistry, c kube.Clients) (*PluginManager, error) {
	return newPluginManager(Catalog(), tr, rr, sr, c)
}

// newPluginManager builds a manager over an explicit plugin list. Tests use it
// to exercise surface shapes the shipped catalog does not have yet.
func newPluginManager(catalog []Plugin, tr *mcp.ToolRegistry, rr *mcp.ResourceRegistry, sr *mcp.SkillRegistry, c kube.Clients) (*PluginManager, error) {
	m := &PluginManager{
		tools:     tr,
		resources: rr,
		skills:    sr,
		plugins:   make([]*pluginState, 0, len(catalog)),
		byName:    make(map[string]*pluginState, len(catalog)),
	}
	for _, p := range catalog {
		st := &pluginState{plugin: p, surface: p.Surface(c)}
		if sr != nil && st.surface.Skills != nil {
			uris, err := skills.URIsIn(st.surface.Skills)
			if err != nil {
				return nil, fmt.Errorf("plugin %q skills: %w", p.Name, err)
			}
			st.skillURIs = uris
		}
		m.plugins = append(m.plugins, st)
		m.byName[p.Name] = st
	}
	return m, nil
}

// RegisterPlugins registers the two always-on plugin meta-tools and returns the
// manager their handlers close over. Every plugin starts disabled: plugin state
// is runtime-only, never seeded from config.
func RegisterPlugins(tr *mcp.ToolRegistry, rr *mcp.ResourceRegistry, sr *mcp.SkillRegistry, c kube.Clients) (*PluginManager, error) {
	m, err := NewPluginManager(tr, rr, sr, c)
	if err != nil {
		return nil, err
	}
	tr.Register(PluginEnableDefinition(m), NewPluginEnableHandler(m))
	tr.Register(PluginDisableDefinition(m), NewPluginDisableHandler(m))
	return m, nil
}

// Names returns every catalog name, in Catalog() order.
func (m *PluginManager) Names() []string {
	out := make([]string, len(m.plugins))
	for i, st := range m.plugins {
		out[i] = st.plugin.Name
	}
	return out
}

// pluginStatusRow is one plugin's entry in every action result: what it is,
// whether it is on, and exactly what it contributes.
type pluginStatusRow struct {
	Name      string   `json:"name"`
	Summary   string   `json:"summary"`
	Enabled   bool     `json:"enabled"`
	Tools     []string `json:"tools"`
	Resources []string `json:"resources,omitempty"`
	Templates []string `json:"templates,omitempty"`
	// Skills names the skill URIs this plugin publishes while enabled. It is
	// here because SEP-2640 defines no skills/list_changed notification: a
	// client that has cached skills/list has no other way to be told a toggle
	// changed the listing.
	Skills []string `json:"skills,omitempty"`
}

type pluginActionOutput struct {
	Plugin string `json:"plugin"`
	// Action is enabled, disabled, already_enabled or already_disabled — the
	// last two saying the call was a no-op, not a failure.
	Action  string            `json:"action"`
	Plugins []pluginStatusRow `json:"plugins"`
}

// enable turns a plugin on, registering its whole surface. Enabling an
// already-enabled plugin is a no-op that deliberately does not re-register:
// Register on an existing name replaces the entry, moves it to the end of the
// list, and fires another list_changed, which would both spam the client and
// churn the ordering the registry keeps stable for client and prompt caching.
func (m *PluginManager) enable(name string) (pluginActionOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.byName[name]
	if !ok {
		return pluginActionOutput{}, fmt.Errorf("%w: %q", ErrUnknownPlugin, name)
	}
	if st.enabled {
		return m.outputLocked(name, "already_enabled"), nil
	}
	if err := m.registerSurfaceLocked(st); err != nil {
		return pluginActionOutput{}, err
	}
	st.enabled = true
	return m.outputLocked(name, "enabled"), nil
}

// disable turns a plugin off, unregistering its whole surface. As with enable,
// a repeat call is a no-op that touches neither registry.
func (m *PluginManager) disable(name string) (pluginActionOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.byName[name]
	if !ok {
		return pluginActionOutput{}, fmt.Errorf("%w: %q", ErrUnknownPlugin, name)
	}
	if !st.enabled {
		return m.outputLocked(name, "already_disabled"), nil
	}
	m.unregisterSurfaceLocked(st.surface, st.skillURIs)
	st.enabled = false
	return m.outputLocked(name, "disabled"), nil
}

// registerSurfaceLocked adds every entry of st's surface to the registries,
// undoing whatever it already added if one fails. RegisterTemplate and the skill
// load are the only calls here that can fail — a malformed URI template is
// reported at registration rather than silently never matching — so this
// rollback is what keeps a plugin from being left half-enabled. Callers hold
// m.mu.
func (m *PluginManager) registerSurfaceLocked(st *pluginState) error {
	s := st.surface
	for _, t := range s.Tools {
		m.tools.Register(t.Def, t.Fn)
	}
	for _, r := range s.Resources {
		m.resources.Register(r.Def, r.Fn)
	}
	for i, t := range s.Templates {
		if err := m.resources.RegisterTemplate(t.Def, t.Fn); err != nil {
			m.unregisterSurfaceLocked(PluginSurface{
				Tools:     s.Tools,
				Resources: s.Resources,
				Templates: s.Templates[:i],
			}, nil)
			return fmt.Errorf("resource template %q: %w", t.Def.URITemplate, err)
		}
	}
	// Skills go last so a load failure rolls back over a fully-known surface.
	// Load registers one skill at a time, so a mid-walk failure can leave
	// earlier ones registered; rolling back every URI the plugin owns covers
	// that, and Unregister is a no-op for a URI that never made it.
	if m.skills != nil && s.Skills != nil {
		if err := skills.Load(m.skills, s.Skills); err != nil {
			m.unregisterSurfaceLocked(s, st.skillURIs)
			return fmt.Errorf("skills: %w", err)
		}
	}
	return nil
}

// unregisterSurfaceLocked removes every entry of s, plus the given skill URIs,
// in the reverse of the order registerSurfaceLocked added them. Callers hold
// m.mu.
func (m *PluginManager) unregisterSurfaceLocked(s PluginSurface, skillURIs []string) {
	if m.skills != nil {
		for _, uri := range skillURIs {
			m.skills.Unregister(uri)
		}
	}
	for _, t := range s.Templates {
		m.resources.UnregisterTemplate(t.Def.URITemplate)
	}
	for _, r := range s.Resources {
		m.resources.Unregister(r.Def.URI)
	}
	for _, t := range s.Tools {
		m.tools.Unregister(t.Def.Name)
	}
}

// outputLocked builds the result both actions return: what just happened, plus
// the current state of the whole catalog. Reporting every plugin is why there
// is no separate list tool — and why the meta-tools' descriptions can stay
// static, since live state travels here instead. Callers hold m.mu.
func (m *PluginManager) outputLocked(name, action string) pluginActionOutput {
	rows := make([]pluginStatusRow, len(m.plugins))
	for i, st := range m.plugins {
		rows[i] = pluginStatusRow{
			Name:      st.plugin.Name,
			Summary:   st.plugin.Summary,
			Enabled:   st.enabled,
			Tools:     surfaceToolNames(st.surface),
			Resources: surfaceResourceURIs(st.surface),
			Templates: surfaceTemplateURIs(st.surface),
			// Cloned rather than aliased: st.skillURIs is the manager's own
			// bookkeeping, and this row is handed to a caller.
			Skills: slices.Clone(st.skillURIs),
		}
	}
	return pluginActionOutput{Plugin: name, Action: action, Plugins: rows}
}

func surfaceToolNames(s PluginSurface) []string {
	out := make([]string, 0, len(s.Tools))
	for _, t := range s.Tools {
		out = append(out, t.Def.Name)
	}
	return out
}

func surfaceResourceURIs(s PluginSurface) []string {
	out := make([]string, 0, len(s.Resources))
	for _, r := range s.Resources {
		out = append(out, r.Def.URI)
	}
	return out
}

func surfaceTemplateURIs(s PluginSurface) []string {
	out := make([]string, 0, len(s.Templates))
	for _, t := range s.Templates {
		out = append(out, t.Def.URITemplate)
	}
	return out
}

// catalogText renders the plugin catalog into the meta-tools' descriptions —
// the whole reason a model can discover plugins without their schemas being
// resident. It reads the same materialized surfaces enable registers, so the
// advertised contents cannot disagree with what enabling actually does.
func (m *PluginManager) catalogText() string {
	var b strings.Builder
	for _, st := range m.plugins {
		fmt.Fprintf(&b, "\n- %s: %s", st.plugin.Name, st.plugin.Summary)
		if names := surfaceToolNames(st.surface); len(names) > 0 {
			fmt.Fprintf(&b, " Adds tools: %s.", strings.Join(names, ", "))
		}
		if uris := surfaceResourceURIs(st.surface); len(uris) > 0 {
			fmt.Fprintf(&b, " Adds resources: %s.", strings.Join(uris, ", "))
		}
		if uris := surfaceTemplateURIs(st.surface); len(uris) > 0 {
			fmt.Fprintf(&b, " Adds resource templates: %s.", strings.Join(uris, ", "))
		}
		// Reads st.skillURIs rather than the surface, so a server not serving
		// skills does not advertise ones that will never appear.
		if len(st.skillURIs) > 0 {
			fmt.Fprintf(&b, " Adds skills: %s.", strings.Join(st.skillURIs, ", "))
		}
	}
	return b.String()
}

// pluginNameSchema builds the shared input schema. The enum is generated from
// the catalog rather than written out, so it can never fall out of step with
// it.
func (m *PluginManager) pluginNameSchema(verb string) json.RawMessage {
	// json.Marshal of a []string cannot fail — no unsupported types, no
	// cycles — so the error is dropped here rather than plumbed through every
	// Definition call site.
	enum, _ := json.Marshal(m.Names())
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"plugin_name": {"type": "string", "enum": %s, "description": "Name of the plugin to %s"}
		},
		"required": ["plugin_name"]
	}`, enum, verb))
}

// pluginActionOutputSchema is shared: both actions report the same shape.
func pluginActionOutputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"plugin": {"type": "string"},
			"action": {"type": "string", "enum": ["enabled", "disabled", "already_enabled", "already_disabled"]},
			"plugins": {
				"type": "array",
				"description": "The state of every plugin, changed or not",
				"items": {
					"type": "object",
					"properties": {
						"name": {"type": "string"},
						"summary": {"type": "string"},
						"enabled": {"type": "boolean"},
						"tools": {"type": "array", "items": {"type": "string"}},
						"resources": {"type": "array", "items": {"type": "string"}},
						"templates": {"type": "array", "items": {"type": "string"}},
						"skills": {"type": "array", "items": {"type": "string"}, "description": "Skill URIs this plugin publishes while enabled; re-read skills/list after a toggle"}
					},
					"required": ["name", "summary", "enabled", "tools"]
				}
			}
		},
		"required": ["plugin", "action", "plugins"]
	}`)
}

// pluginMetaAnnotations annotates the two meta-tools. Not readOnly(): they
// change which tools this server exposes. Not destructive either — they touch
// no cluster, and disabling only withdraws tools that enabling puts back.
func pluginMetaAnnotations() *mcp.ToolAnnotations {
	notDestructive := false
	return &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &notDestructive}
}

// PluginEnableDefinition describes the kubectl_plugin_enable tool.
//
// This and PluginDisableDefinition take an argument, unlike every other
// XxxDefinition in this package: they are the only tools whose description and
// input schema are generated from the catalog rather than written as literals.
func PluginEnableDefinition(m *PluginManager) mcp.Tool {
	return mcp.Tool{
		Name:  "kubectl_plugin_enable",
		Title: "Enable a plugin",
		Description: "Enables an optional plugin: its tools appear in tools/list immediately and this server " +
			"notifies you that the tool list changed. Enable a plugin only when you need it — every tool's " +
			"schema costs context for the rest of the session — and disable it again with " +
			"kubectl_plugin_disable when you are done. Enabling a plugin that is already enabled is a " +
			"harmless no-op. A plugin that adds skills publishes them on enable, and the result names " +
			"them: re-read skills/list afterwards, since there is no skills-changed notification. " +
			"Plugins available:" + m.catalogText(),
		InputSchema:  m.pluginNameSchema("enable"),
		OutputSchema: pluginActionOutputSchema(),
		Annotations:  pluginMetaAnnotations(),
	}
}

// NewPluginEnableHandler returns the kubectl_plugin_enable handler.
func NewPluginEnableHandler(m *PluginManager) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		return m.handleAction(req, m.enable)
	}
}

// PluginDisableDefinition describes the kubectl_plugin_disable tool.
func PluginDisableDefinition(m *PluginManager) mcp.Tool {
	return mcp.Tool{
		Name:  "kubectl_plugin_disable",
		Title: "Disable a plugin",
		Description: "Disables a plugin enabled earlier by kubectl_plugin_enable: its tools disappear from " +
			"tools/list and this server notifies you that the tool list changed. Nothing on any cluster is " +
			"touched and nothing is lost — re-enabling restores the same tools. Disabling a plugin that is " +
			"already disabled is a harmless no-op. Any skills the plugin published are withdrawn too. " +
			"Plugins available:" + m.catalogText(),
		InputSchema:  m.pluginNameSchema("disable"),
		OutputSchema: pluginActionOutputSchema(),
		Annotations:  pluginMetaAnnotations(),
	}
}

// NewPluginDisableHandler returns the kubectl_plugin_disable handler.
func NewPluginDisableHandler(m *PluginManager) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		return m.handleAction(req, m.disable)
	}
}

// handleAction is the shared body of both meta-tools: bind, act, report. Every
// failure is a tool error naming the valid plugins, since that is the one thing
// a model needs to recover.
func (m *PluginManager) handleAction(req *mcp.ToolRequest, action func(string) (pluginActionOutput, error)) (mcp.Result, error) {
	var args struct {
		PluginName string `json:"plugin_name"`
	}
	if err := req.BindArguments(&args); err != nil {
		return mcp.ErrorResultf("invalid arguments: %v", err), nil
	}
	if args.PluginName == "" {
		return mcp.ErrorResultf("the plugin_name argument is required; the available plugins are: %s",
			strings.Join(m.Names(), ", ")), nil
	}

	out, err := action(args.PluginName)
	if err != nil {
		if errors.Is(err, ErrUnknownPlugin) {
			return mcp.ErrorResultf("unknown plugin %q; the available plugins are: %s",
				args.PluginName, strings.Join(m.Names(), ", ")), nil
		}
		// Only a malformed resource template, or a skill colliding with one
		// another plugin already published, reaches here — a defect in the
		// plugin rather than anything the caller did. But the caller is who is
		// waiting, so say so plainly.
		return mcp.ErrorResultf("plugin %q could not be changed because its own definition is invalid: %v",
			args.PluginName, err), nil
	}
	return jsonResult(out, nil)
}
