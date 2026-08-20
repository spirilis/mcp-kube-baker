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
