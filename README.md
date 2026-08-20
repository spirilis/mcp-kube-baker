# mcp-kube-baker

A read-only Kubernetes MCP server built on
[generic-go-mcp](https://github.com/spirilis/generic-go-mcp) (MCP protocol
**2026-07-28**) and `k8s.io/client-go`. It exposes one or more clusters —
selected by kubeconfig context — to MCP clients such as Claude Code and
Claude Desktop.

## Quick start

```bash
go build ./cmd/mcp-kube-baker

# Zero config: stdio transport, legacy MCP compat enabled, logging to stderr
# (info/text), kubeconfig from $KUBECONFIG or ~/.kube/config.
./mcp-kube-baker

# Or point it somewhere explicit:
./mcp-kube-baker --kubeconfig /path/to/kubeconfig
./mcp-kube-baker --config config.yaml
```

Register it with Claude Code:

```bash
claude mcp add kube-baker -- /path/to/mcp-kube-baker
```

Kubeconfig precedence: `--kubeconfig` flag > `kubeconfig:` in the config file
> `$KUBECONFIG` (first entry if a list) > `~/.kube/config`.

## Tools

Every tool that touches a cluster is read-only, and every one except
`kubectl_get_contexts` takes a `context` argument naming the kubeconfig context
(cluster) to query. The two `kubectl_plugin_*` tools below are the one exception
to "read-only": they touch no cluster, but they do change which tools this server
serves — see [Plugins](#plugins).

| Tool | Arguments | Returns |
|---|---|---|
| `kubectl_get_contexts` | — | context names + current-context |
| `kubectl_get_namespaces` | `context` | namespace names |
| `kubectl_get_pods` | `context`, `namespaces?` (array) | per pod: namespace, pod_name, pod_ip, node_name, status_phase, containers, init_containers |
| `kubectl_get_pod_logs` | `context`, `namespace`, `pod`, `container?`, `previous?`, `tail_lines?`, `max_size_kib?`, `timestamps?` | the tail of one container's stdout/stderr, plus the window that was applied |
| `kubectl_get_events` | `context`, `namespace?` | per event: count, firstTimestamp, namespace, name, reason, message, type |
| `kubectl_get_nodes` | `context` | per node: name, kubernetes_version, labels, allocatable, capacity, addresses |
| `kubectl_get_services` | `context`, `namespaces?` (array) | per service: namespace, name, selector, type, ipFamilies, ipFamilyPolicy, traffic policies, ports |
| `kubectl_get_api_resources` | `context` | per Kind: name (plural), kind, api_group, short_names, versions, preferred_version, namespaced, verbs — CRDs included |
| `kubectl_get_helm_installs` | `context`, `namespace?` | per Helm release: namespace, name, revision, status, chart, chart_version, app_version, updated, description, storage |
| `kubectl_get_arbitrary_manifest` | `context`, `api_group?`, `api_version`, `kind`, `name`, `namespace?` | the object's full JSON manifest, plus what the Kind resolved to |
| `kubectl_plugin_enable` | `plugin_name` | exposes that plugin's tools; returns the state of every plugin |
| `kubectl_plugin_disable` | `plugin_name` | withdraws them again; returns the state of every plugin |

### Log windows

`kubectl_get_pod_logs` takes its container names from `kubectl_get_pods` (init
and ephemeral containers work too); `container` may be omitted only when the pod
has exactly one, and otherwise the error names the candidates. `previous: true`
is `kubectl logs -p`. `timestamps` defaults to **on**, which is what makes log
lines correlatable with `kubectl_get_events` output.

The tail window is the caller's to choose, and the two bounds are independent:

| `tail_lines` | `max_size_kib` | Result |
|---|---|---|
| set | set | the last N lines, then the last M KiB of that |
| — | set | the whole retained log streamed through a ring buffer → exactly the last M KiB |
| set | — | exactly the last N lines, untrimmed |
| — | — | the last 256 KiB, no line limit |

The Kubernetes API's `limitBytes` truncates from the *start* of the stream, so
it is never used here — a true tail is trimmed client-side, dropping the leading
partial line so the output begins at a line boundary. A trimmed result says so
in its first line and sets `truncated` in `structuredContent`.

Omitting the optional namespace argument(s) queries all namespaces. Results
carry both a JSON text block and `structuredContent`, plus `resource_link`
entries (capped at 100) pointing at the full manifests below.

## Plugins

Every tool definition a server advertises sits in the model's context for the
whole session, so tools for add-ons a given cluster may not even run are a
standing cost. Plugins move that cost behind a switch: a plugin's tools stay out
of `tools/list` until a client asks for them.

```
kubectl_plugin_enable(plugin_name)    → the plugin's tools appear
kubectl_plugin_disable(plugin_name)   → they go away again
```

Both tools' *descriptions* carry the full catalog — name, what it is, and exactly
what enabling it adds — so discovery costs a few lines rather than every plugin's
schemas. `plugin_name` is a schema `enum` generated from that same catalog, and
every call returns the state of all plugins, so there is no third tool to ask
"what's on?".

Enabling or disabling registers or unregisters against the live registry, which
makes the server send `notifications/tools/list_changed` (over
`subscriptions/listen` for 2026-07-28 clients, in the classic push shape for
legacy-compat sessions). Repeating a call is a deliberate no-op: it neither
re-registers nor re-notifies.

| Plugin | Adds | Needs |
|---|---|---|
| `karpenter` | `kubectl_karpenter_logs`, `skill://karpenter-nodes/SKILL.md` | Karpenter installed, and `get`/`list` on `coordination.k8s.io/v1` leases |

Nothing is enabled at startup. Plugin state is runtime-only — there is no config
key and no state file, so every process starts clean.

Two things to know:

- **State is per process, not per client.** Under the HTTP transport every
  connection shares one registry, so one client enabling `karpenter` makes its
  tools appear for all of them. That is correct for stdio, where a client gets
  its own process, and it is the intended model there.
- **A plugin may also contribute resources, resource templates, and skills**, not
  just tools; the registry and its `resources/list_changed` notification are
  symmetric with the tool side. `karpenter` contributes a tool and a skill.
  Skills are the one asymmetric case, because the extension defines no
  skills-changed notification — see [Skills](#skills-experimental). With
  `skills.enabled: false` a plugin's tools still come and go as normal; only its
  skills are absent, and it advertises none.

### karpenter

`kubectl_karpenter_logs(context, namespace?, container?, previous?, tail_lines?,
max_size_kib?, timestamps?)` returns the logs of the Karpenter replica that
*currently holds leadership* — the only one doing any provisioning, and so the
only one whose logs explain why nodes are or are not appearing.

It finds that pod rather than asking for it: read the `karpenter-leader-election`
Lease in `kube-system` (override with `namespace`; a renamed or forked install is
matched by any Lease there whose name mentions karpenter, and the one used is
reported back), take its `holderIdentity` — controller-runtime writes
`<pod-name>_<uuid>` — and resolve that to a pod, falling back to a cluster-wide
search by name for installs whose Lease and pods live in different namespaces.
The container defaults to `controller` when present, else the pod's only one.

If the Lease has expired the logs are still returned, with `stale: true`, a reason,
and a leading banner: a Karpenter that stopped renewing leadership is usually
exactly what you are looking into. The window arguments behave identically to
[`kubectl_get_pod_logs`](#log-windows).

**A "does not appear to be installed" result is not a bug.** The tool starts from
the leader-election Lease, so a cluster without Karpenter correctly reports that
there is none.

Enabling the plugin also publishes `skill://karpenter-nodes/SKILL.md`, the
procedure for using this tool: what the staleness banner means, and how to tell
"Karpenter never saw the pod" from "the cloud provider refused" from "the node
never joined" — three failures that look identical from a Pending pod's side.

## Resources (templates)

Full object manifests are served as parameterized resources — RFC 6570
templates advertised via `resources/templates/list`:

| Template | Content |
|---|---|
| `mcp+kubectl://{context}/pod/{namespace}/{name}` | full Pod JSON |
| `mcp+kubectl://{context}/event/{namespace}/{name}` | full Event JSON |
| `mcp+kubectl://{context}/node/{node}` | full Node JSON |
| `mcp+kubectl://{context}/service/{namespace}/{name}` | `{"items": [Service, EndpointSlice...]}` |
| `mcp+kubectl://{context}/helm-values/{namespace}/{release}` | the release's user-supplied values (`helm get values`) + the chart's defaults |
| `mcp+kubectl://{context}/api-resource/{apiGroup}/{version}/{kind}` | OpenAPI V3 schema of one Kind + every schema it references |
| `mcp+kubectl://{context}/api-resource/{version}/{kind}` | the same, for core (`v1`) Kinds |

The core API group's name is the empty string, which no URI path segment can
hold, so core Kinds are addressed either as `core` (`.../api-resource/core/v1/Pod`)
or through the group-less short template (`.../api-resource/v1/Pod`) — both
return the same document. `{kind}` also accepts the plural, singular, or short
name, so `Deployment`, `deployments`, and `deploy` all resolve.

An api-resource read returns a self-contained mini OpenAPI document: the Kind's
component schema plus the transitive closure of its `$ref`s (every reference
inside it resolves), `x-kubernetes-root-schema` naming which component the URI
asked for, and an `x-kubernetes-api-resource` block carrying the discovery
metadata. Returning the whole group-version document instead would be roughly
four times larger and mostly about other Kinds.

A nonexistent object (or unknown context) answers with JSON-RPC `-32602`
("Unknown resource"), never empty contents. `completion/complete` is
supported for the `{context}` variable (kubeconfig context names), for
`{namespace}` once the client has resolved a context, for
`{apiGroup}`/`{version}`/`{kind}` from cluster discovery, and for `{release}`
once a context and namespace are resolved; object names are an unbounded
keyspace and return no completions.

## Skills (experimental)

Troubleshooting procedures are shipped as embedded [Agent
Skills](https://agentskills.io/specification), served over the
`io.modelcontextprotocol/skills` MCP extension: `skills/list`, `skills/get`, and
`resources/directory/read`. The point of serving a skill over MCP rather than
writing a runbook is that these procedures name *this server's* tools, arguments
and resource URIs — generic `kubectl` advice names commands a read-only server
cannot execute.

| Skill | Available | Covers |
|---|---|---|
| `skill://pod-triage/SKILL.md` | always | A pod not Running or not Ready: `kubectl_get_pods` → the pod manifest resource → `kubectl_get_pod_logs` with `previous: true` → `kubectl_get_events`. Bundles `references/failure-modes.md`, a symptom → next-step table for `CrashLoopBackOff`, `OOMKilled`, `Pending`/`FailedScheduling`, `Init:Error` and the rest. |
| `skill://karpenter-nodes/SKILL.md` | while the `karpenter` plugin is enabled | Karpenter node provisioning: reading the leader's logs through `kubectl_karpenter_logs`, what the staleness banner means, and telling a NodePool misconfiguration from a capacity shortfall. |

A skill's files are registered as **ordinary resources** on the same registry the
`mcp+kubectl://` templates live on, so `resources/read` can serve exactly the
bytes `skills/list` published a `sha256:` digest for — the two can never disagree
about what a skill contains. They are distinguished from live cluster data only
by scheme, which is intended.

`resources/directory/read` is enabled, so a client can list `skill://pod-triage`
to discover its bundled references instead of parsing them out of the listing.
Nothing else becomes visible through it: it walks only registered concrete
resources, and every cluster-data resource here is a template.

Plugin-scoped skills come and go with their plugin, which is the one wrinkle
worth knowing about. Enabling a plugin publishes its skill and fires
`notifications/resources/list_changed` (a skill's files being resources), but the
extension defines **no** skills-changed notification — so a client holding a
cached `skills/list` is not told to refetch it. The `kubectl_plugin_enable`
result therefore names the skill URIs it published; re-read `skills/list` after a
toggle.

Skills are on by default and can be turned off with `skills.enabled: false`,
which makes the extension absent rather than empty: no capability key, and all
three methods answer `-32601`. **Turning skills off does not turn plugins off** —
a plugin still contributes its tools, and simply advertises no skills.

> **Experimental.** This implements
> [SEP-2640](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2640),
> an MCP Extensions Track proposal that is **open and not merged**. Nothing about
> skills appears in the ratified 2026-07-28 specification, and no public MCP
> client consumes it yet. It ships anyway because it is purely additive and
> invisible to any client that does not ask: a server with skills off is
> byte-for-byte the old one, and — unlike a tool's schema, which sits in the
> model's context all session — a skill's body is never loaded until a host
> explicitly calls `skills/get`. A breaking revision of the SEP would be a
> breaking change to a feature nobody was required to adopt. Requires
> `generic-go-mcp` v0.8.0 or later.

## Helm

Helm releases are read straight out of the cluster: Helm 3 keeps one record per
revision in a `helm.sh/release.v1` Secret (or, under `HELM_DRIVER=configmap`, a
ConfigMap) labelled `owner=helm`, holding the release JSON gzipped and
base64-encoded. Both drivers are consulted, and only the current revision of
each release is reported — what `helm list` shows. No Helm binary or
`helm.sh/helm/v3` dependency is involved, so nothing here can install, upgrade,
or roll anything back.

A `helm-values` read returns `user_supplied_values` (what `helm get values`
prints) and `chart_default_values` side by side rather than merged. Helm's
coalescing rules — subchart scoping, null-means-delete, globals — are subtler
than a deep merge, so a merge computed here would be a plausible-looking lie in
exactly the cases where it matters.

Reading releases means reading Secrets. A kubeconfig context whose credentials
cannot list Secrets gets an error from this tool and nothing else; one that can
will surface whatever values were supplied at install time, secrets included.

## Arbitrary resource types

`kubectl_get_arbitrary_manifest` fetches any single object of any Kind the
cluster serves — CustomResourceDefinitions and the custom resources they define
included — the way `kubectl get <kind> <name> -o json` does. The Kind is
resolved through discovery, so `Certificate`, `certificates`, `certificate`, and
`cert` all work, and the result records what the arguments resolved to.

The manifest comes back as the API server sent it, minus
`metadata.managedFields` — server-side apply bookkeeping is roughly a third of a
typical object's bytes and says nothing about it, which is why `kubectl get -o
json` hides it too.

`api_version` takes either the bare version (`v1`, with the group in
`api_group`) or the qualified `group/version` form (`apps/v1`, with `api_group`
omitted). Core Kinds take `api_group: core` or no group at all. Namespaced
Kinds require `namespace`; cluster-scoped Kinds must omit it, and getting that
wrong is a tool error naming which way it went rather than a silent empty
result.

## Configuration

The config file itself is optional — without `--config`, the defaults above
apply (note that legacy compat is **on** in zero-config mode but **off** by
default in a config file).

```yaml
kubeconfig: /home/user/.kube/config   # optional — falls back to $KUBECONFIG / ~/.kube/config; never inlined
server:
  mode: stdio                         # stdio | http | unix
  http:                               # http mode only
    host: 127.0.0.1
    port: 8080
    allowed_origins: []               # empty = localhost origins only; ["*"] = any
  legacy_compat:                      # serve pre-2026-07-28 clients too
    enabled: false
    session_ttl: 30m
auth:                                 # GitHub OAuth, http mode only
  enabled: false
skills:                               # experimental skills extension (SEP-2640)
  enabled: true                       # default; false hides the extension entirely
logging:
  level: info                         # info | debug | trace
  format: text                        # text | json
```

See `config.yaml` (stdio) and `config-http.yaml` (HTTP + OAuth + legacy
compat) for complete annotated examples. The `server`, `auth`, and `logging`
sections are the generic-go-mcp library's own config schema; `kubeconfig` and
`skills` are this server's own.

Notes:

- **Auth** (GitHub OAuth) applies to the HTTP transport only. When enabled,
  responses are marked `cacheScope: private` automatically.
- **Legacy compat** wraps the server in generic-go-mcp's `compat.Overlay`,
  restoring the `initialize` handshake and (on HTTP) `Mcp-Session-Id`
  sessions for 2025-11-25-and-earlier clients. The skills extension is
  forwarded to those clients too — the capability appears in the `initialize`
  result and frontmatter and digests survive the result downgrade — on the
  reasoning that a legacy client which understands it can use it, and one that
  does not ignores an unknown capability key.
- **Skills** are embedded at build time, so this knob only chooses whether to
  serve them; there is nothing to point at a directory. See
  [Skills](#skills-experimental).
- On the 2026-07-28 protocol, every request must carry
  `params._meta["io.modelcontextprotocol/protocolVersion"]` and
  `.../clientCapabilities`; HTTP requests additionally need the
  `MCP-Protocol-Version`, `Mcp-Method`, and (for calls/reads) `Mcp-Name`
  headers.

## Development

```bash
go build ./... && go vet ./... && go test ./...
```

Unit tests run against `k8s.io/client-go/kubernetes/fake` and
`k8s.io/client-go/dynamic/fake` clientsets — no cluster required.
