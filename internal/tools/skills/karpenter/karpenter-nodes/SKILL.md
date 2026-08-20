---
name: karpenter-nodes
description: Diagnose Karpenter node provisioning — pods that stay Pending when they should have triggered a new node — using mcp-kube-baker's karpenter plugin, whose log tool resolves the current leader-election holder for you.
license: Apache-2.0
compatibility: Provided by mcp-kube-baker's karpenter plugin; requires kubectl_karpenter_logs, which exists only while that plugin is enabled.
metadata:
  version: 1.0.0
---

# Karpenter node diagnosis

You are reading this because the `karpenter` plugin is enabled, which means
`kubectl_karpenter_logs` is in `tools/list`. That tool is the reason this
procedure differs from ordinary pod triage: Karpenter runs several replicas but
only the one holding the leader-election Lease does any provisioning, so reading
"the Karpenter logs" means reading the *leader's* logs, and the tool resolves
which pod that is on every call.

This server is read-only. It cannot create a NodePool, drain a node, or delete a
NodeClaim. The deliverable is a diagnosis.

## Steps

1. **Confirm the symptom is Karpenter's.** Call `kubectl_get_pods` with
   `context` and look for pods with `status_phase: "Pending"` and no
   `node_name`. Then call `kubectl_get_events` with `context` and find their
   `FailedScheduling` events. That message is the input Karpenter acts on: it
   names the resource, taint, or selector the pod needs. A pod Pending for a
   reason Karpenter cannot fix by adding a node — an unsatisfiable node
   selector, a missing PVC, a zone with no capacity offering — is not a
   provisioning failure.

2. **Read the leader's logs.** Call `kubectl_karpenter_logs` with `context`. The
   remaining arguments mirror `kubectl_get_pod_logs` exactly — `namespace`,
   `container`, `previous`, `tail_lines`, `max_size_kib`, `timestamps` — but
   `pod` is absent by design: the tool finds the leader itself from the
   leader-election Lease's `holderIdentity`. Do not go hunting for the right
   replica with `kubectl_get_pods` and `kubectl_get_pod_logs`; you will pick a
   standby and see nothing.

   Look for the provisioning decision for the pod from step 1: Karpenter logs the
   pod it is scheduling, the NodeClaim it creates, and the instance type it
   chose. Silence about a pod that has a `FailedScheduling` event is the
   strongest signal available — it means Karpenter never considered it, which
   points at step 4.

3. **Mind the staleness banner.** If the result carries an `EXPIRED` banner and a
   `stale_reason`, the Lease has not been renewed. Those logs are the *last
   known* leader's, and may predate the problem you are looking at — treat every
   timestamp in them with that in mind, and say so in your report. A stale Lease
   with no live holder is itself a finding: Karpenter is not provisioning at all,
   and no amount of NodePool inspection will explain the Pending pods.

   If the leader pod itself restarted mid-provisioning, call again with
   `previous: true` to read the run that died. An OOM-killed leader is a common
   cause of provisioning that stops silently.

4. **Check the provisioning configuration.** Karpenter's own objects are custom
   resources, so read them with `kubectl_get_api_resources` to confirm the group
   and version this cluster serves, then `kubectl_get_arbitrary_manifest` for the
   `NodePool` and its `EC2NodeClass` (or the equivalent for your cloud
   provider). A NodePool whose requirements exclude every instance type that
   would satisfy the pod, or whose `limits` are already reached, produces exactly
   the "Karpenter ignored my pod" symptom from step 2.

5. **Check whether nodes arrived.** Call `kubectl_get_nodes` with `context` and
   look for `karpenter.sh/nodepool` and `karpenter.sh/registered` among the
   labels. A node that Karpenter created but that never became Ready is a
   bootstrap failure, not a provisioning failure — read
   `mcp+kubectl://{context}/node/{node}` for its taints and
   `status.conditions`, since the node list carries neither. An unremoved
   `node.cloudprovider.kubernetes.io/uninitialized` taint or a lingering
   `karpenter.sh/unregistered` taint means the node joined but was never
   finished.

6. **Report**, distinguishing the three outcomes that look identical from the
   pod's side: Karpenter never saw the pod (step 4 — configuration), Karpenter
   tried and the cloud provider refused (step 2's logs — capacity or quota), or
   Karpenter succeeded and the node failed to join (step 5 — bootstrap). Each has
   a different owner.

## When you are done

If you enabled the plugin only for this investigation, call
`kubectl_plugin_disable` with `plugin_name: "karpenter"` to take its tools —
and this skill — back out of the session.
