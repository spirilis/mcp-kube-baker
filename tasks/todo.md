# mcp-kube-baker — Implementation TODO

Plan references:
- `~/.claude/plans/get-the-initial-plan-swift-yeti.md` (approved 2026-08-19)
- `~/.claude/plans/check-out-the-new-declarative-fern.md` (v0.7.0 update, approved 2026-08-19)

## Phase 0 — Library plan document
- [x] Write `~/src/generic-go-mcp/PLAN-adding-parameterized-resources-engine.md`
- [x] Library v0.7.0 shipped: templates + completion + compat pass-through (tag pushed)

## Phase 1 — Scaffold
- [x] `go.mod`: bump to generic-go-mcp v0.7.0, drop the `replace` directive
- [x] `internal/config` — YAML config: required external `kubeconfig` path + library sections
- [x] `internal/kube` — ClientManager: context enumeration + lazy cached per-context clientsets
- [x] `cmd/mcp-kube-baker/main.go` — config → registries → transport wiring, stdio default
- [x] Tool: `kubectl_get_contexts`
- [x] Build + stdio smoke test

## Phase 2 — Remaining tools
- [x] `kubectl_get_namespaces(context)`
- [x] `kubectl_get_pods(context, namespaces?)`
- [x] `kubectl_get_events(context, namespace?)`
- [x] `kubectl_get_nodes(context)`
- [x] `kubectl_get_services(context, namespaces?)`
- [x] Unit tests with fake clientset (arg validation, multi-namespace merge, unknown-context error)

## Phase 3 — HTTP transport, auth, compat, docs
- [x] HTTP transport wiring (host/port/allowed_origins)
- [x] Optional GitHub OAuth (`auth.enabled`; conditional AuthService assignment; `DefaultCacheScope=private` when on)
- [x] Legacy compat opt-in (`compat.Overlay` + `LegacySessions` on HTTP; widen `AdvertisedVersions`)
- [x] Example `config.yaml` + `config-http.yaml` + README
- [x] HTTP smoke test (curl with MCP-Protocol-Version / Mcp-Method / Mcp-Name headers)

## Phase 4 — Resources (v0.7.0 template engine)
- [x] `mcp+kubectl://{context}/pod/{namespace}/{name}` — full Pod JSON
- [x] `mcp+kubectl://{context}/event/{namespace}/{name}` — full Event JSON
- [x] `mcp+kubectl://{context}/node/{node}` — full Node JSON
- [x] `mcp+kubectl://{context}/service/{namespace}/{name}` — Service + EndpointSlices items[]
- [x] `mcp.ErrResourceNotFound` for missing objects and unknown contexts (never empty contents)
- [x] ResourceLink content entries in tool outputs (capped at 100)

## Phase 4a — Completion (NEW in v0.7.0)
- [x] `context` completer — kubeconfig context names, in-memory
- [x] `namespace` completer — uses resolved `context` from `context.arguments`; empty otherwise
- [x] No completion for `name`/`node` (unbounded)

## Zero-config launch (added 2026-08-19, user request)
- [x] `config.Default()` — stdio, legacy compat ON, info/text logging to stderr
- [x] Kubeconfig precedence: `--kubeconfig` flag > config file > `$KUBECONFIG` (first list entry) > `~/.kube/config`, validated once on the final path (`config.ValidateKubeconfig`)
- [x] `--config` optional; config files may omit `kubeconfig:` (falls back to env/home)
- [x] Unit tests (`internal/config/config_test.go`) + smoke matrix: zero-config call, legacy initialize with no config, flag-over-file override, omitted-kubeconfig fallback, `KUBECONFIG=/nonexistent` error naming what was tried

## Review (2026-08-19)

All phases complete. Layout: `cmd/mcp-kube-baker/main.go`, `internal/config`,
`internal/kube` (ClientManager over one external kubeconfig), `internal/tools`
(6 read-only tools), `internal/resources` (4 templates + shared completer).

Verification performed:
- `go build ./... && go vet ./... && go test ./...` — all green; tests use
  `kubernetes/fake` clientsets (no cluster needed). `gofmt` clean.
- stdio smoke test against the real `~/.kube/config`: server/discover
  (capabilities tools+resources+completions), kubectl_get_contexts matches
  `kubectl config get-contexts` (apollo/apollo-remote/linode/rpi4, current
  linode), resources/templates/list returns 4 templates, read of an unknown
  context URI → `-32602`, context completion returns all 4 names.
- Live-cluster reads against `linode`: kubectl_get_pods(auth-system) → 4 pods
  + 4 resource links; pod resource read returns full Running manifest;
  service read returns Service + its one EndpointSlice (label-filtered);
  namespace completion with resolved context → `auth-system`.
- HTTP smoke test on 127.0.0.1:18321: modern tools/call with the three
  required headers works; legacy `initialize` through compat.Overlay gets an
  Mcp-Session-Id and a 2025-06-18 downgraded result; missing
  MCP-Protocol-Version header → `-32020`.

Not committed to git — awaiting user review.

---

## Phase 7 — api-resources tool + resource (2026-08-19)

Closes the last unimplemented pair in `PLAN.md:47-50`.

- [x] `internal/kube/manager.go` — `Clients.OpenAPI(context)`, memoized per
      context and wrapped in `client-go/openapi/cached`
- [x] `internal/tools/apiresources.go` — `kubectl_get_api_resources`
- [x] `internal/tools/tools.go` — registration + `apiResourceURI`
- [x] `internal/resources/apiresources.go` — two templates, Kind resolution,
      OpenAPI V3 `$ref` closure extraction
- [x] `internal/resources/resources.go` — registration + `apiGroup`/`version`/
      `kind` completions
- [x] Tests on both packages
- [x] README

### Design notes

**Two templates, not one.** The core API group's name is the empty string, and
`generic-go-mcp` compiles `{var}` to `([^/]+)` (`mcp/templates.go:141`) — it
never matches an empty segment, so `.../api-resource//v1/Pod` is unmatchable.
Core is reachable both as the `core` sentinel and through a group-less short
template. Their segment counts differ (three versus two after `api-resource`),
so the registry's first-match-wins scan cannot confuse them; both produce
byte-identical documents.

**Schema closure, not the whole document.** A read returns the Kind's component
schema plus the transitive closure of its `$ref`s, keyed as in the source so
every reference still resolves. Measured live: Pod 134 components / 244 KB,
Deployment 120 / 221 KB, against ~1 MB+ for the full group-version document,
most of which is about other Kinds. `x-kubernetes-root-schema` names which
component the URI asked for; `x-kubernetes-api-resource` carries the discovery
metadata.

**`x-kubernetes-group-version-kind` is a list.** Shared kinds (`DeleteOptions`,
`WatchEvent`) carry dozens of GVK entries, so the seed lookup scans every entry
rather than indexing `[0]`.

**`OpenAPI` had to go on the interface.** `FakeDiscovery.OpenAPIV3()` panics
`"unimplemented"` (`client-go@v0.36.3 discovery/fake/discovery.go:168`), so
reaching it through `kubernetes.Interface` would have left the whole schema
path untestable. Tests inject `openapitest.NewEmbeddedFileClient()`.

**Partial discovery failure is not a tool failure.** One unreachable aggregated
APIService populates `warnings` and leaves the rest of the catalog intact, the
way `kubectl api-resources` behaves. Anything else is still a tool error.

### Verification

`go build`, `go vet`, `gofmt`, `go test ./...` all green — 39 tests, no cluster
needed. Live stdio run against `~/.kube/config`, context `linode`:

- `kubectl_get_api_resources` → 108 Kinds including CRDs (cert-manager, Gateway
  API, kagent.dev); zero subresources (`pods/status`, `deployments/scale`
  filtered); multi-version Kinds collapsed correctly with the server's preferred
  version (`autoscaling/HorizontalPodAutoscaler [v1 v2] pref=v2`,
  `gateway.networking.k8s.io/Gateway [v1 v1beta1] pref=v1`); 100 resource links
  (the cap), core ones in `core` form.
- `resources/templates/list` → 6 templates with `resultType`/`ttlMs`/
  `cacheScope`.
- Reads of `apps/v1/Deployment`, `core/v1/Pod`, `v1/Pod`, `apps/v1/deploy`,
  `cert-manager.io/v1/Certificate`, `gateway.networking.k8s.io/v1/HTTPRoute` all
  succeed. `core/v1/Pod` and `v1/Pod` return byte-identical text; `deploy`
  resolves to Deployment. Every `$ref` in all six documents resolves inside the
  document (119/133/5/5 refs, 0 dangling).
- Not-found: `apps/v1/Nope`, `bogus.io/v1/Thing`, `v1/Nope`, and an unknown
  context all answer `-32602` with an error object, never empty contents.
- Completions: `apiGroup` prefix `gate` → 3 groups; `version` for
  `gateway.networking.k8s.io` → 4; `kind` prefix `HTTP` → `HTTPRoute`; and the
  short template's own `version`/`kind` (`Serv` → Service, ServiceAccount).
- Caching: 6 reads of the same URI in one process cost 0.46 s against 0.40 s for
  one — the extra five together cost ~12 ms versus ~400 ms for the first, so the
  group-version document is fetched once.

Not committed to git — awaiting user review.

---

## Phase 8 — Helm installs + arbitrary manifest retrieval (2026-08-19)

Closes the final unimplemented items in `PLAN.md:52-60`.

- [x] `internal/kube/apiresource.go` — shared `GroupVersion` / `NormalizeGroup` /
      `ResolveAPIResource` (lifted out of `internal/resources/apiresources.go`
      so the new tool and the api-resource resource agree on Kind spelling)
- [x] `internal/kube/manager.go` — `Clients.Dynamic(context)`, memoized per
      context alongside the clientset and OpenAPI client
- [x] `internal/helm` — Helm 3 storage-record decoding (base64 → gzip → JSON)
      from Secrets and ConfigMaps, no `helm.sh/helm/v3` dependency
- [x] Tool: `kubectl_get_helm_installs(context, namespace?)`
- [x] Resource: `mcp+kubectl://{context}/helm-values/{namespace}/{release}`
      + `release` completion
- [x] Tool: `kubectl_get_arbitrary_manifest(context, api_group?, api_version,
      kind, name, namespace?)`
- [x] Tests on all four packages (72 total, no cluster needed)
- [x] README

### Design notes

**No `helm.sh/helm/v3` dependency.** Helm's release record is a documented,
stable storage contract: JSON, gzipped, base64'd, under the `release` key of a
`helm.sh/release.v1` Secret labelled `owner=helm`. Decoding it is ~100 lines
(`internal/helm/release.go`). Importing the Helm module to do the same would
pull chart rendering, repository handling and a second Kubernetes client stack
into a read-only MCP server — and would make it possible for this binary to
install and roll back, which it must never be able to do.

**Both storage drivers.** Secrets are Helm 3's default, but
`HELM_DRIVER=configmap` installs exist and are invisible if only Secrets are
consulted; each row reports which driver held it. A ConfigMap list that fails
(a context allowed to read Secrets but not ConfigMaps) degrades to a warning
rather than failing the tool.

**Timestamps must tolerate `""`.** Helm marshals an unset `info.deleted` as
`0001-01-01T00:00:00Z`, but records written by other tooling carry `""` or
`null`, which `encoding/json`'s `time.Time` rejects outright — throwing away an
otherwise readable release over a field nothing displays. `helm.Time` accepts
all three.

**Values are shown unmerged.** `user_supplied_values` (exactly what `helm get
values` prints) sits beside `chart_default_values`, not merged into it. Helm's
coalescing rules — subchart scoping, null-means-delete, globals — are subtler
than a deep merge, so a merge computed here would be a plausible-looking lie in
precisely the cases where the answer matters.

**Only the current revision.** Every revision has its own record;
`latestPerRelease` collapses them by highest `version`, which is what `helm
list` shows. `Latest` trusts the release document's own name over the record's
`name` label, so a mislabelled record cannot answer for a release it does not
hold.

**Kind resolution moved to `internal/kube`.** The arbitrary-manifest tool and
the api-resource resource must agree that `deploy`, `deployments`, `deployment`
and `Deployment` are one Kind. The shared resolver reports its two failures as
`ErrUnknownGroupVersion` / `ErrUnknownKind`; the resource surface maps both to
`mcp.ErrResourceNotFound` (-32602), the tool surface to sentences naming the
next tool to call. It also fills `Group`/`Version` back into the returned
`APIResource` — the per-group-version discovery list omits them, and a caller
building a GVR needs them.

**Scope mismatch is an error in both directions.** A namespaced Kind without a
namespace, and a cluster-scoped Kind with one, each get a message naming which
way it went. Silently ignoring the namespace on a cluster-scoped Get would make
`Node` in namespace `default` look like it existed there.

**`api_version` accepts `apps/v1`.** Every manifest a model has read spells the
group and version together, so both forms work; a contradicting `api_group` is
an error rather than a silent winner.

**`metadata.managedFields` is stripped** from the arbitrary manifest — measured
at 34% of a live cert-manager Certificate (5267 → 3476 bytes) and describing
nothing about the object. `kubectl get -o json` hides it by default for the
same reason. The four full-manifest resource templates still include it; worth
revisiting together if that noise shows up in practice.

### Verification

`go build`, `go vet`, `gofmt`, `go test ./...` and `go test -race` all green —
72 tests, `kubernetes/fake` + `dynamic/fake`, no cluster needed. Live stdio run
against `~/.kube/config`, context `linode`:

- `tools/list` → 9 tools; `resources/templates/list` → 7 templates.
- `kubectl_get_helm_installs(linode)` → 8 releases, exactly matching `helm list
  -A` row for row (name, namespace, revision, chart-version, app version,
  timestamp), with 8 `helm-values` resource links and no warnings.
  Namespace-scoped call (`kagent`) → the 2 releases there.
- `helm-values` read of `cert-manager/cert-manager` → revision 2,
  `user_supplied_values` byte-identical to `helm get values cert-manager -n
  cert-manager`, `chart_default_values` 42 keys alongside it. Absent release,
  wrong namespace, and unknown context all answer `-32602`, never empty
  contents. `release` completion with prefix `ka` → `kagent`, `kagent-crds`.
- `kubectl_get_arbitrary_manifest` matrix (11 calls): CRD instance
  (`cert-manager.io/v1 Certificate`) by Kind and by short name `cert`;
  cluster-scoped CRD (`ClusterIssuer`); core `Node`; core `cm` short name with
  `api_group: core`; qualified `apps/v1`. The Certificate manifest is
  byte-identical to `kubectl get certificate dovecot-tls -n auth-system -o
  json`. Errors: missing namespace on `Deployment`, namespace on `Node`,
  `bogus.io/v1`, unknown kind `Nope`, absent object, and contradicting
  `api_group`/`api_version` — each a tool error with a recoverable message,
  none a protocol error.
- Legacy compat (zero-config default): `initialize` → 2025-06-18, `tools/list`
  → 9 tools with no `ttlMs`, a legacy `tools/call` of
  `kubectl_get_helm_installs` returns the same rows, templates list → 7.

Not committed to git — awaiting user review.

---

## Phase 9 — pod container names + logs tool (2026-08-19)

Plan reference: `~/.claude/plans/note-the-different-return-temporal-pebble.md`
(approved 2026-08-19).

- [x] `PLAN.md` — `containers[]`/`init_containers[]` on the `kubectl_get_pods`
      return; the two `pod-logs` / `pod-logs-prev` **resources replaced by** the
      `kubectl_get_pod_logs` **tool**, with the reasoning recorded inline
- [x] `internal/tools/pods.go` — `containers` + `init_containers` (names only)
      on every row, shared `containerNames` helper
- [x] `internal/tools/podlogs.go` — `kubectl_get_pod_logs` + the `tailBuffer`
      ring writer
- [x] `internal/tools/tools.go` — registration (10 tools)
- [x] `internal/resources/` — deliberately unchanged; still 7 templates
- [x] Tests: `podlogs_test.go` (13 cases incl. the four window combinations and
      the ring writer) + `TestGetPodsReportsContainerNames`
- [x] README

### Design notes

**Logs are a tool, not a resource.** `PLAN.md` originally spec'd `pod-logs` and
`pod-logs-prev` as resource templates. Resources are host-driven — a human picks
a URI out of a picker — and most hosts never let the model read a URI it derived
itself. Logs are wanted at exactly the moment the model has just found a
crash-looping pod through `kubectl_get_pods`, so they have to be model-reachable.
This is the `PLAN.md:191-206` rule applied, not an exception to it: the full
manifests stay resources because they are the stable, citable artifacts; a log
tail is neither. `previous` then falls out as a boolean argument rather than a
second URI shape.

**`LimitBytes` is the wrong end of the stream.** `PodLogOptions.LimitBytes`
truncates from the *start* of what the API would send, and `TailLines` counts
lines, not bytes — so no combination of them can answer "the last N KiB".
`tailBuffer` reads the stream and drops what falls out of the window, compacting
only past twice the limit so the copy is amortized and memory stays bounded at
2×limit. `max_size_kib` is ceilinged at 8192 because the window is allocated
from a caller-supplied number.

**Truncation trims to a line boundary.** A byte window lands mid-line, and a
leading fragment of a JSON log line is worse than useless to a reader. The
leading partial line is dropped and the fact is reported twice — a `[truncated:
…]` banner on the text block and `truncated` in `structuredContent` — because a
model that cannot tell a short log from a clipped one will draw conclusions from
the absence of earlier lines.

**Timestamps default on**, unlike `kubectl logs`. The kubelet's RFC3339Nano
prefix is what lets a log line be lined up against `kubectl_get_events` output;
an app's own timestamps are in whatever format and timezone it chose, if any.

**The pod is fetched before the log request.** It costs one API call and buys
three better errors: a missing pod is named as such, a single-container pod
needs no `container` argument, and a wrong container name comes back with the
real candidates (init and ephemeral containers included, since both have logs)
instead of the API's bare rejection.

**The result is not built through `jsonResult`.** That helper's text block is the
JSON encoding of the output, which would escape every newline in the log and
leave it unreadable; `kubectl_get_pod_logs` puts the raw log in the text block
and keeps the metadata in `structuredContent`.

### Verification

`go build`, `go vet`, `gofmt`, `go test ./...` and `go test -race ./...` all
green — 86 tests, `kubernetes/fake` with a reactor on the `pods/log`
subresource, no cluster needed. Live stdio run against `~/.kube/config`,
context `linode`:

- `tools/list` → 10 tools; `resources/templates/list` → still 7 (no log
  templates).
- `kubectl_get_pods(linode, ["auth-system"])` → `containers` and
  `init_containers` match `kubectl get pods -o jsonpath` row for row, including
  the 2-container `spam-rspamd-0` and the 2-init-container `postfix-0`.
- Default window on `dovecot-0` → 261997 bytes kept of 3601329 streamed,
  **byte-identical** to `kubectl logs --timestamps | tail -c 262144` after the
  147-byte leading partial line both drop.
- Window matrix on the same pod: `tail_lines:20` → exactly 20 lines, 5089 bytes,
  untruncated (matches `kubectl logs --tail=20`); `max_size_kib:8` → 8004 bytes,
  truncated, banner present, starts at a line boundary; both (`20` + `1 KiB`) →
  930 bytes of the 5089-byte 20-line tail; `timestamps:false` → no stamps.
  `LimitBytes` is never set in any of them.
- `previous:true` on the restarted `kagent-controller-…` → byte-identical to
  `kubectl logs -p`; on the never-restarted `dovecot-0` → a tool error naming
  what previous means, not a protocol error.
- Errors, all recoverable tool errors: multi-container pod with no `container`
  (names `[rspamd logtail]`), unknown container (names the real ones plus the
  init containers), missing pod (points at `kubectl_get_pods`), unknown context
  (points at `kubectl_get_contexts`), `tail_lines:0`, `max_size_kib:0`,
  `max_size_kib:8193`.
- Init and second containers are selectable: `combine-certs` (init) and
  `logtail` both return their own logs.
- Legacy compat (zero-config default): `initialize` → 2025-06-18, `tools/list`
  → 10 tools with no `ttlMs`, a legacy `tools/call` of `kubectl_get_pod_logs`
  returns the same text with `structuredContent` intact.

Not committed to git — awaiting user review.
