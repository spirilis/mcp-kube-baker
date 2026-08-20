Architecture:

Using client-go as much as possible, implementing tooling and resources to expose an MCP client (nominally Claude Code
or Claude Desktop) to 1 or more Kubernetes clusters, selectable by altering the kubeconfig context.

Use the `generic-go-mcp` library and implement stdio & HTTP transports.  Support OAuth2/OIDC authentication for HTTP
transport mode.  Support legacy MCP mode from the library.

This tool's CLI launcher will support a config .yaml file with the necessary configuration, including a path in the
config to a valid KUBECONFIG file.  That KUBECONFIG must be external to the config.yaml file.

Initial tooling:

Tool: kubectl_get_contexts() - return list of available contexts
Tool: kubectl_get_namespaces($context) - return list of namespaces on that context (cluster)
Tool: kubectl_get_pods($context, [$namespace | slice($namespaces)]) - Get pods on a cluster, optionally in 1 namespace,
  or in a set of namespaces, or if the namespace argument is omitted (it's optional), get ALL pods in that cluster
  Values expected for each Pod:
  namespace, pod_name, pod_ip, node_name, status_phase, containers[] (just the names),
  init_containers[] (just the names, omitted when the Pod has none)

Resource: mcp+kubectl://$context/pod/$namespace/$pod_name - Get full information on a Pod, responding with the JSON
  object of the Pod.

Tool: kubectl_get_pod_logs($context, $namespace, $pod, [$container], [$previous], [$tail_lines], [$max_size_kib],
  [$timestamps]) - Get a container's stdout/stderr logs.  $container may be omitted when the Pod has exactly one
  container; otherwise the error names the candidates.  $previous (default false) reads the previously executed
  container, for a Pod that has restarted.  $timestamps (default true) prefixes each line with the kubelet's
  RFC3339Nano stamp, which is what makes these lines correlatable with kubectl_get_events output.
  The tail window is the caller's to choose, and $tail_lines / $max_size_kib are independent:

    $tail_lines + $max_size_kib -> the API returns the last N lines; the last M KiB of that is kept
    $max_size_kib alone         -> the whole retained log is streamed through a ring buffer -> exactly the last M KiB
    $tail_lines alone           -> exactly the last N lines, untrimmed
    neither                     -> $max_size_kib defaults to 256, no line limit

  The Kubernetes API's limitBytes truncates from the START of the stream, so it is never used here; a true tail has
  to be trimmed client-side.

  Logs are a Tool and not a Resource, unlike the full manifests above, for the reason the "template vs. tool" table
  further down states: resources are host-driven -- a human picks a URI out of a picker -- while logs are wanted
  exactly when the model has just found a crash-looping Pod through kubectl_get_pods and must fetch them itself,
  without a human in the loop.  Making $previous an argument rather than a second URI shape follows from the same
  choice.

Tool: kubectl_get_events($context, [$namespace]) - Get Events on the cluster, optionally for a specific namespace (get
  events for all namespaces if $namespace is omitted).
  Fields to respond with:
  count, firstTimestamp, metadata.namespace, metadata.name, reason, message, type

Resource: mcp+kubectl://$context/event/$namespace/$name - Get the full JSON manifest of a particular Event

Tool: kubectl_get_nodes($context) - Get the full list of Node's in a cluster
  Fields to respond with:
  name, kubernetes_version, .metadata.labels map, .status.allocatable map, .status.capacity map, .status.addresses map

Resource: mcp+kubectl://$context/node/$node - Get the full JSON output of a particular Node

Tool: kubectl_get_services($context, [$namespace | slice($namespaces)]) - Get a list of Service objects in the cluster
  Fields to respond with:
  namespace, name, selector map, type, ipFamilies, ipFamilyPolicy, internalTrafficPolicy, externalTrafficPolicy, .spec.ports map

Resource: mcp+kubectl://$context/service/$namespace/$service_name - Get the full configuration of a Service AND all
  its EndpointSlices
  Fields to respond with:
  An .items[] array of JSON documents with the JSON of the Service and all the JSONs for the EndpointSlice objects
  belonging to it

Tool: kubectl_get_api_resources($context) - List all the api-resources with shortnames, apiGroup, available versions, Kind names

Resource: mcp+kubectl://$context/api-resource/$apiGroup/$version/$kind - Obtain the full OpenAPI V3 or Swagger schema for
  this API resource.

Tool: kubectl_get_helm_installs($context, $namespace) - List helm chart installations and their chart versions

Resource: mcp+kubectl://$context/helm-values/$namespace/$release - List the `helm get values` output for this helm chart schema.

Tool: kubectl_get_arbitrary_manifest($context, [$namespace], $apiGroup, $apiVersion, $kind, $name) - Retrieve any arbitrary
  resource-type from the K8s API ($namespace is optional for Cluster Global resources; for Namespaced resources, this
  will return an error).  Result is the JSON document for the resource itself.


Plugins (added 2026-08-20):

Tool: kubectl_plugin_enable($plugin_name) / kubectl_plugin_disable($plugin_name) - Expose or withdraw an optional
  family of tools at runtime.  Both tools are always registered, and both DESCRIPTIONS carry the whole plugin
  catalog -- name, one-line summary, and exactly what enabling it adds -- so a model can discover every plugin
  without any of their schemas being resident.  $plugin_name is a schema enum generated from the same catalog.
  Each call returns the state of every plugin, which is why there is no third "list" tool: tools/list is the list,
  and the action result covers the one thing it cannot say.

  Why this exists: every tool definition a server advertises stays in the model's context for the whole session.
  Compiling in diagnostics for every Kubernetes add-on would tax that context on clusters running none of them.
  A plugin is therefore a declarative catalog entry, not a compilation unit -- plugin tools live in
  internal/tools alongside the core ones and reuse the same helpers.

  A plugin surface carries tools, concrete resources AND resource templates. mcp.ResourceRegistry is symmetric
  with mcp.ToolRegistry -- Register/Unregister, RegisterTemplate/UnregisterTemplate -- and both fire their
  registry's onChange, which mcp.NewServer wires to the subscription broker. So no notification code is written
  here at all: enabling a plugin is a Register call, and connected clients get notifications/tools/list_changed
  (or resources/list_changed) for free.  tools/list and resources/templates/list read the registries live per
  request, so nothing is snapshotted at startup.

  Two rules a plugin's templates must follow, both inherited from the registry: MatchTemplate is
  first-match-wins in registration order and a plugin's templates land after the core ones, so a plugin must
  claim its own URI shape (mcp+kubectl://{context}/karpenter/...) rather than one a core template already
  swallows; and only {var} and a terminal {+var} compile.

  Repeating a call is a deliberate no-op.  Register on a name already present replaces the entry, moves it to
  the end of the list, and fires another list_changed -- so a second enable must not re-register, or every
  repeat call would spam the client and churn the ordering the registry keeps stable for client and prompt
  caching.  For the same reason the generated descriptions are built once and never regenerated: rendering live
  "(enabled)" state into them would require re-registering the meta-tools on every toggle.

  Plugin state is runtime-only -- no config key, no state file -- and per process rather than per client.  Under
  the HTTP transport one registry is shared by every connection, so one client's enable changes every client's
  tool list.  That is correct for stdio, which is the default and gives each client its own process.

  This does not violate 2026-07-28's rule that a tool set MUST NOT vary per-connection or as a side effect of
  another request.  The library's own design note states the carve-out inline: "registry mutation by the embedder
  is fine -- that's what list_changed is for".  The rule targets HIDDEN variation.  No ordinary tool here touches
  the registry; only the two dedicated, model-visible meta-tools do.

Plugin: karpenter

Tool: kubectl_karpenter_logs($context, [$namespace], [$container], [$previous], [$tail_lines], [$max_size_kib],
  [$timestamps]) - The logs of the Karpenter replica that CURRENTLY HOLDS LEADERSHIP: the only one doing any
  provisioning, and so the only one whose logs explain why nodes are or are not appearing.

  The active pod is found rather than asked for.  Read the karpenter-leader-election Lease in kube-system
  ($namespace overrides that), take its .spec.holderIdentity -- controller-runtime writes
  <pod-name>_<uuid>, and "_" is not legal in a pod name, so the first one is an unambiguous split -- and
  resolve it to a pod.  Three fallbacks cover installs that differ from the default: any Lease in that
  namespace whose name merely mentions karpenter (the one used is reported back, so a fuzzy match is never
  silent), a cluster-wide search by pod name when the Lease's namespace is not the pod's, and a container
  named "controller" preferred over any sidecar.

  An expired Lease returns the logs anyway, with stale=true, a reason, and a leading banner -- a Karpenter
  that stopped renewing leadership is usually exactly what is being investigated, and its last lines are why.
  A Lease missing renewTime or leaseDurationSeconds reports "freshness unknown" rather than implying it is
  current.

  The window arguments are identical to kubectl_get_pod_logs, sharing its implementation: no bounds means the
  last 256 KiB with a truncation banner, and $tail_lines alone applies no byte cap -- which is how a caller
  asks for the whole log.  An unbounded default would be the opposite of what a context-management feature is
  for.

  A "does not appear to be installed" result is not a defect.  The tool starts from the leader-election Lease,
  so a cluster without Karpenter correctly reports that there is none -- verify installation before treating a
  live failure as a bug.


Skills (added 2026-08-20, EXPERIMENTAL -- SEP-2640, requires generic-go-mcp v0.8.0):

Method: skills/list, skills/get, resources/directory/read - Serve embedded Agent Skills (SKILL.md with YAML
  frontmatter, plus whatever files sit beside it) behind the io.modelcontextprotocol/skills capability
  extension.  internal/skills owns the skill:// prefix and every loading operation; skill content lives in
  internal/skills/content/<name>/ for the always-on set and internal/tools/skills/<plugin>/<name>/ for a
  plugin's, on the principle that content belongs to whatever owns its lifetime.

  Why serve skills at all, when a runbook in the README would do: a procedure written against THIS server's
  tool names, argument names and resource URIs is the thing a generic Kubernetes-savvy model cannot supply
  itself.  Generic kubectl advice names commands a read-only server cannot execute.  So pod-triage says
  "kubectl_get_pod_logs with previous: true", not "kubectl logs -p", and names
  mcp+kubectl://{context}/pod/{namespace}/{name} as where lastState.terminated lives.  That specificity is the
  entire value, and it is also the entire fragility -- see the doc-rot guard below.

  Cost profile, and why skills are NOT gated the way plugin tools are: a tool's schema sits in the model's
  context for the whole session, which is what plugins exist to avoid.  A skill's body is never loaded until a
  host calls skills/get; skills/list carries only frontmatter and digests.  So there is no context-budget
  argument for hiding a skill, and the always-on set is simply always on.

  Plugin-scoped skills (PluginSurface.Skills fs.FS) exist for a different reason: relevance.  A Karpenter
  procedure is noise on a cluster that does not run Karpenter, and it reads better assuming the plugin is
  already enabled -- discovery is kubectl_plugin_enable's job, since its description already advertises the
  catalog.  An fs.FS rather than a []mcp.SkillDef so the library's walk rules, frontmatter parsing and URI
  escaping remain the only implementation of themselves.  skills.URIsIn loads a plugin's tree into a throwaway
  registry at PluginManager construction, which both validates it at startup (embedded content can only be
  wrong by build defect, so failing at a client's first enable call would be the wrong time) and learns the
  exact URIs disable must undo -- from LoadFS itself, so there is no second derivation path to drift.

  The one asymmetry with tools: SEP-2640 defines no skills/list_changed, and the library deliberately did not
  invent one.  A skill's files ARE ordinary resources, so publishing one fires resources/list_changed -- but
  nothing tells a client that skills/list itself changed.  Hence pluginStatusRow.Skills: the enable/disable
  result names the URIs, which is the only in-band signal available.  Verified on the wire: enable emits
  exactly one tools/list_changed and one resources/list_changed (not one per file), and a repeat enable emits
  neither.

  A footgun the library cannot catch: registering a skill under a URI another skill already holds is treated
  as replacement, not conflict.  Two plugins -- or a plugin and the always-on set -- sharing a skill directory
  name would silently shadow each other, and disabling one would withdraw the other's content.
  TestSkillURIsAreUniqueAcrossTiers guards it.  Relatedly, a plugin must never claim a skill:// URI through
  Resources/Templates, since SkillRegistry only detects conflicts between skills.

  The doc-rot guard is the load-bearing test.  Prose does not compile, so a renamed tool or retired template
  would leave the skills confidently wrong with nothing in the build objecting.
  TestSkillContentReferencesRealToolsAndResources extracts every kubectl_* identifier and every
  mcp+kubectl:// URI from every shipped skill -- always-on and plugin-scoped alike -- and resolves them against
  a live ToolRegistry (RegisterAll + the meta-tools + every plugin's catalog surface, enabled or not) and a
  live ResourceRegistry via MatchTemplate.  The URI half works because the skills write URIs in the same
  {context} placeholder style the tool descriptions use: MatchTemplate matches structurally on segment count
  and literal segments, so the literal text {context} binds to the {context} variable.  Its own failure was
  demonstrated by deliberately breaking both a tool name and a URI before trusting it.

  Config: skills.enabled, defaulting to TRUE.  A *bool, for the same reason kubectl_get_pod_logs takes one for
  $timestamps -- an omitted field has to be distinguishable from an explicit false.  False makes the extension
  absent rather than empty: ServerConfig.Skills stays nil, no capability is declared, all three methods answer
  -32601, and no skill:// resource is registered.  Turning skills off does not turn plugins off.

  Scheme: skill://, which SEP-2640 makes a SHOULD rather than a MUST.  Taken anyway so a skills-aware host
  recognizes it, and because it separates static documentation from live cluster objects in resources/list --
  the two share one registry by design, since that is what makes skills/list and resources/read incapable of
  disagreeing about a skill's contents.

  resources/directory/read is enabled so a client can list skill://<name> to find bundled references rather
  than parsing them out of the listing.  It exposes nothing else: DirectoryChildren walks only registered
  CONCRETE resources, and every cluster-data resource here is a template.

  Kept honest: the extension is opt-in upstream and experimental here.  SEP-2640 is Extensions Track, open and
  not merged; no public client consumes it.  It ships because it is purely additive -- a server with skills off
  is byte-for-byte the old one -- so a breaking SEP revision breaks a feature nobody was required to adopt.



About the use of Resources here:
(from Claude's investigation into MCP 2026-07-28 and parametric Resource URLs)

# MCP Resource Templates (spec 2026-07-28)

Reference for implementing/consuming parameterized resources. Applies to the
`2026-07-28` revision; notes legacy deltas where they matter for `2025-11-25` mode.

## Core answer

Resource URIs **are** parameterized via RFC 6570 URI templates. A server never
enumerates the full URI space. It advertises templates; the client expands them
locally and calls `resources/read` with a concrete URI.

- `resources/list` → concrete, enumerable resources (small, curated sets).
- `resources/templates/list` → template families (unbounded keyspaces).
- Server does **not** expand. Expansion is client-side.

## Wire shapes

`resources/templates/list` response:

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "result": {
    "resultType": "complete",
    "resourceTemplates": [
      {
        "uriTemplate": "genome://{genome}/rsid/{rsid}",
        "name": "Marker by rsID",
        "title": "Marker by rsID",
        "description": "Genotype for a single rsID",
        "mimeType": "application/json"
      }
    ],
    "nextCursor": "optional",
    "ttlMs": 300000,
    "cacheScope": "public"
  }
}
```

Read is unchanged in shape — concrete URI only:

```json
{ "method": "resources/read", "params": { "uri": "genome://hg38/rsid/rs12345" } }
```

## 2026-07-28 requirements (compliance checklist)

- [ ] `ttlMs` (ms freshness hint) and `cacheScope` (`"public"` | `"private"`)
      are **required** on `tools/list`, `prompts/list`, `resources/list`,
      `resources/read`, and `resources/templates/list` (`CacheableResult`).
- [ ] `resultType: "complete"` on results.
- [ ] Resource/template sets **MUST NOT** vary per-connection or as a side
      effect of another request. They **MAY** vary by the authorization
      presented on the request (creds are per-request input, not session state).
- [ ] Resource-not-found is `-32602` (was `-32002`). Clients SHOULD still accept
      `-32002` from older servers. Internal errors → `-32603`.
- [ ] Never return an empty `contents: []` for a nonexistent resource — ambiguous;
      error instead.
- [ ] Every request carries `_meta`: `io.modelcontextprotocol/protocolVersion`,
      `io.modelcontextprotocol/clientInfo`, `io.modelcontextprotocol/clientCapabilities`.
- [ ] Subscriptions: `subscriptions/listen` with `notifications.resourceSubscriptions`,
      not the old `resources/subscribe` + GET-SSE stream.
- [ ] Optional: `resources/read` MAY return an `InputRequiredResult` (MRTR) to
      demand a missing variable; client retries with `inputResponses` and any
      server-supplied `requestState`.
- [ ] `capabilities.resources` has only `listChanged` and `subscribe`. There is
      **no** "templates supported" flag — clients probe `resources/templates/list`
      and treat `-32601` as unsupported.

## Narrowing a large keyspace: `completion/complete`

Templates describe shape, not contents. Autocompletion is the only protocol
mechanism for exploring a large variable domain.

```json
{
  "method": "completion/complete",
  "params": {
    "ref": { "type": "ref/resource", "uri": "genome://{genome}/rsid/{rsid}" },
    "argument": { "name": "rsid", "value": "rs123" },
    "context": { "arguments": { "genome": "hg38" } }
  }
}
```

- `context.arguments` carries previously-resolved template variables — use it to
  constrain later variables.
- Response caps at 100 values, with `total` / `hasMore`.
- Requires declaring the `completions` capability.
- Back it with a prefix index; never materialize the keyspace.

## RFC 6570 gotchas

**Expansion is one-way.** RFC 6570 defines template → URI only. Matching a
concrete URI back to a template and extracting bindings is *not* in the spec and
must be implemented. In Go:

- `github.com/yosida95/uritemplate/v3` — has non-standard `Regexp()` / `Match()`;
  the pragmatic choice when you need reverse matching.
- `std-uritemplate` — faithful Level 4 expansion, expansion only.

**Pick a low level unless you own the client.** Many clients implement Level 1
only.

| Form | Level | Note |
|---|---|---|
| `{var}` | 1 | Simple string; percent-encodes `/` |
| `{+var}` | 2 | Reserved expansion; use for multi-segment paths |
| `{#var}` | 2 | Fragment |
| `{?a,b}` | 3 | Query params |
| `{/seg*}` | 4 | Path explode; least portable |

`file:///{path}` only "works" because clients are lax about encoding. For real
multi-segment values use `{+path}`.

**Ordering:** register longest/most-specific templates first if your matcher is
first-match-wins, or `genome://{g}/rsid/{r}` will shadow
`genome://{g}/rsid/{r}/detail`.

## Design rule: template vs. tool

Resources are **application-driven** — the host decides how to surface them
(picker UI, search/filter, heuristic context inclusion). Tools are
**model-driven** — the model calls them directly with derived arguments.

| Access pattern | Use |
|---|---|
| Model derives an identifier mid-reasoning and needs the record | **Tool** |
| Human browses/selects from a namespace in host UI | **Resource template** |
| Stable, citable, linkable artifact the host may cache | **Resource template** |
| Unbounded keyspace, no interactive picker in the host | **Tool** |

Implement templates for the linkable/citable surface, but do not expect them to
replace a lookup tool. Most hosts will not exercise them.

## Legacy (`2025-11-25`) deltas to gate on

- No `ttlMs` / `cacheScope` / `resultType` — omit them.
- Not-found is `-32002`.
- `resources/subscribe` + HTTP GET SSE instead of `subscriptions/listen`.
- `initialize` / `notifications/initialized` handshake and `Mcp-Session-Id`
  still present; no per-request `_meta` protocol version.
- No `InputRequiredResult` / MRTR.

## Sources

- <https://modelcontextprotocol.io/specification/2026-07-28/server/resources>
- <https://modelcontextprotocol.io/specification/2026-07-28/changelog>
- RFC 6570 — <https://datatracker.ietf.org/doc/html/rfc6570>
