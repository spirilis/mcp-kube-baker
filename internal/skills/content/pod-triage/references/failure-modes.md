# Pod failure modes

Each row is a symptom you can see from `kubectl_get_pods` or the pod manifest,
what it actually means, and which step of `SKILL.md` settles it. Read this after
step 3, once you have `status.containerStatuses[]` in hand.

| Symptom | What it means | Where the answer is |
| --- | --- | --- |
| `state.waiting.reason: CrashLoopBackOff` | The container started, exited, and the kubelet is now delaying the next restart. The exit already happened — this reason describes the *waiting*, not the failure. | Step 4 with `previous: true`. `lastState.terminated.exitCode` and the previous run's log are the whole story; the current log is startup noise. |
| `state.waiting.reason: ImagePullBackOff` / `ErrImagePull` | The image reference or the registry credentials are wrong. The container has never run, so there are no logs. | Step 5. The `Failed` event's message names the registry error — not found, unauthorized, or a DNS/proxy failure — and distinguishes a typo from a missing pull secret. |
| `lastState.terminated.reason: OOMKilled` (or `exitCode: 137`) | The kernel killed the container for exceeding its memory limit. Not an application crash, though a leak can cause it. | Step 3 for `spec.containers[].resources.limits.memory`, then step 4 with `previous: true` to see whether the workload was mid-request or idle when it died. |
| `exitCode: 1` with application output | An ordinary application failure. | Step 4 with `previous: true`. Read the last lines before the exit, not the first. |
| `exitCode: 0` but restarting | The process completed successfully and the restart policy brought it back. Usually a container that should have been a Job, or a missing foreground process. | Step 3 for `spec.restartPolicy`. |
| `status_phase: Pending`, `node_name` empty | Never scheduled. No logs exist. | Step 6. The `FailedScheduling` event message states the unmet requirement verbatim; `status.conditions[PodScheduled]` carries the same reason. |
| `status_phase: Pending`, `node_name` set | Scheduled but not started — usually pulling the image, or waiting on a volume attach. | Step 5. Look for `Pulling`/`Pulled` and `FailedAttachVolume`/`FailedMount` events. |
| `Init:Error` / `Init:CrashLoopBackOff` | An init container failed, so no app container has started. The app container's logs will be empty and that is expected, not a second problem. | Step 4, passing the failing **init** container's name (from `init_containers`) as `container`. |
| `Init:0/2` staying put | An init container is running and not finishing — commonly waiting on a dependency that will never arrive. | Step 4 on the init container, without `previous` — it has not terminated. |
| `state.waiting.reason: CreateContainerConfigError` | A referenced ConfigMap, Secret, or key does not exist. The container was never created. | Step 3 for the `envFrom`/`valueFrom`/`volumes` reference, then step 5 for the event naming the missing object. |
| `state.waiting.reason: CreateContainerError` | The runtime refused to create the container — a bad command, entrypoint, or mount. | Step 5. The event message carries the runtime's own error. |
| Running, `ready: false`, `restartCount: 0` | A readiness probe is failing. The process is alive and its own log may look perfectly healthy. | Step 5 for `Unhealthy` events, which name the probe and the HTTP status or exit code, then step 3 for the probe definition. |
| Running, `ready: false`, `restartCount` climbing | A *liveness* probe is failing and killing the container. | Step 5 for `Unhealthy` events, then step 4 with `previous: true` to see what the process was doing when it was killed. |
| `status_phase: Failed`, `reason: Evicted` | The kubelet evicted the pod under node pressure (memory, disk, or pid). | Step 5 for the eviction event, then step 6 — this is a node-level problem, not a pod-level one. |
| Stuck `Terminating` | Deletion is blocked, almost always by a finalizer or an unresponsive volume detach. | Step 3 for `metadata.finalizers` and `metadata.deletionTimestamp`. This server cannot clear a finalizer; report it. |
| `status_phase: Unknown` | The node stopped reporting. The pod's real state is unobservable from here. | Step 6. Check the node — this is a node or network problem. |

## Two things worth not concluding

- **No events is not "nothing happened."** Events expire (an hour by default in
  most clusters). A pod that has been crash-looping all week may have no events
  left describing the first failure.
- **Logs are the container's, not the pod's.** `previous: true` reads the last
  terminated instance of one container. If the container has been restarted many
  times, everything before the previous run is gone from the API — the pod
  manifest's `restartCount` is the only remaining evidence of the earlier ones.
