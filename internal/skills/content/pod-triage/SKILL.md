---
name: pod-triage
description: Diagnose a Kubernetes pod that is not Running or not Ready, using mcp-kube-baker's own read-only tools and resources — the exact call sequence, argument names and resource URIs for this server, not generic kubectl advice.
license: Apache-2.0
compatibility: Requires mcp-kube-baker's kubectl_get_pods, kubectl_get_pod_logs and kubectl_get_events tools and its mcp+kubectl:// pod, event and node resources.
metadata:
  version: 1.0.0
---

# Pod triage

A pod is stuck, crashing, or not Ready. This is the order to ask this server's
tools about it, and the arguments that matter at each step.

This server is **read-only**. It cannot restart, patch, scale, delete, or exec
into anything. The deliverable is a diagnosis and a recommended action for a
human to take — never an attempt to fix.

## Steps

1. **Name the cluster.** Every tool takes a `context` argument. If you do not
   already know which cluster is meant, call `kubectl_get_contexts` (it takes no
   arguments) and use `current_context` unless the user named another.

2. **Find the pod.** Call `kubectl_get_pods` with `context`, and `namespaces`
   (an array) if the namespace is known — omit it to search the whole cluster.
   Three fields in each row drive everything that follows:
   - `status_phase` — `Pending`, `Running`, `Succeeded`, `Failed`, `Unknown`.
     Note that a pod crash-looping is still phase `Running`; the phase alone
     never proves health.
   - `containers` and `init_containers` — these are exactly the names
     `kubectl_get_pod_logs` accepts. An init container that never completes is a
     common cause of a pod that looks stuck with no application logs at all.
   - `node_name` — empty means the pod was never scheduled, which sends you to
     step 6 rather than to the logs.

   The result carries a `resource_link` per pod. Follow that link rather than
   assembling the URI yourself.

3. **Read the manifest.** Fetch
   `mcp+kubectl://{context}/pod/{namespace}/{name}`. The pod list is a summary;
   the manifest is where the reason lives:
   - `status.containerStatuses[].state` — the current state and, for a waiting
     container, the `reason` (`CrashLoopBackOff`, `ImagePullBackOff`,
     `CreateContainerConfigError`, ...).
   - `status.containerStatuses[].lastState.terminated` — `reason`, `exitCode`,
     `finishedAt` for the **previous** run. `OOMKilled` here, or exit code 137,
     is a memory limit, not an application bug.
   - `status.containerStatuses[].restartCount` — a nonzero count is what makes
     step 4's `previous` argument the interesting one.
   - `status.conditions` — the `PodScheduled: False` entry carries the
     scheduler's own explanation for a `Pending` pod.

   `references/failure-modes.md` maps each reason to the step that resolves it.

4. **Read the logs.** Call `kubectl_get_pod_logs` with `context`, `namespace` and
   `pod` (all required) and `container` — omit `container` only when the pod has
   exactly one. Init and ephemeral container names are accepted too.

   The decisive argument is `previous`. Set `previous: true` whenever
   `restartCount` is nonzero or `lastState.terminated` is set: the running
   container's log cannot explain why the *last* one died, and for a
   crash-looping pod the current log is usually a few seconds of startup noise.
   Read both — the previous run for the failure, the current one for whether it
   is repeating identically.

   The window arguments are `tail_lines` (bound by lines) and `max_size_kib`
   (bound by bytes); with neither, you get the last 256 KiB. Leave `timestamps`
   alone — it defaults to true, and those RFC3339Nano prefixes are what make the
   log correlatable in step 5. `truncated: true` in the result means the front of
   the log was dropped, so raise `tail_lines` if the interesting line is above
   the window.

5. **Correlate with events.** Call `kubectl_get_events` with `context` and
   `namespace` (omit `namespace` for the whole cluster). Match `reason` and
   `message` against the log timestamps from step 4. Events explain what the
   kubelet and scheduler did — image pulls, probe failures, evictions — which is
   the half of the story the container's own stdout never contains. Full event
   manifests are at `mcp+kubectl://{context}/event/{namespace}/{name}` when the
   summary message is truncated.

   Events are short-lived. An empty list is not evidence that nothing happened.

6. **If the pod never scheduled** (`status_phase: "Pending"` with no
   `node_name`): the logs do not exist yet, so skip steps 4 and 5's log
   correlation. Look for a `FailedScheduling` event — its message states the
   unmet requirement verbatim ("Insufficient cpu", an untolerated taint, no
   matching node selector). Then check `kubectl_get_nodes` for the cluster's
   `allocatable` figures, and read
   `mcp+kubectl://{context}/node/{node}` for a candidate node's taints and
   `status.conditions`, which the node list does not carry.

7. **Report.** State the phase, the restart count, the container state reason,
   the terminated exit code, the events that corroborate it, and one concrete
   recommendation. Say plainly which of those you could not determine — an
   expired log or an aged-out event is a gap in the evidence, not a conclusion.
