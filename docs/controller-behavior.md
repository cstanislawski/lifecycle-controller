# Controller behavior

## Configuration flags

By default, the controller watches resources across all namespaces. It skips subresources and APIs that do not support `get`, `list`, `watch`, `patch`, and `delete`. Discovery fails if a `--watch-resource` pattern selects an API without these verbs, unless `--ignore-resource` excludes it.

Resource filters select which API types to watch. Namespace filters limit deletions and restarts. Exact namespace watch lists also restrict reads of namespaced resources. Watch lists with glob patterns require reads across all namespaces.

Use these flags to select resources and namespaces:

- `--watch-resource` (repeatable) - select resource types with a glob pattern.
  - Use `<resource>.<group>` for grouped APIs (e.g., `deployments.apps`) or `<resource>` for core APIs (e.g., `pods`).
  - If omitted, watch all eligible resources except those excluded by `--ignore-resource`. The pattern `*.*` does not match core resources such as `pods`.
  - Use exact resource names to avoid selecting unsupported APIs.
- `--ignore-resource` (repeatable) - exclude resource types with a glob pattern, overrides `--watch-resource`.
- `--watch-namespace` (repeatable) - limit deletions and restarts to namespaces that match a glob pattern (e.g., `default` or `dev-*`).
  - To act on Namespace objects, also set `--watch-resource=namespaces`. Other cluster-scoped resources are excluded when this flag is set.
- `--ignore-namespace` (repeatable) - exclude namespaces with a glob pattern, overrides `--watch-namespace`.

Use these flags to set the default policies for missed actions:

- `--restart-catch-up` - set the policy for missed restarts. Accepts `never`, `always`, or a positive duration (e.g., `15m`). Default: `always`. Helm value: `controllerManager.actionTiming.restart.catchUp`.
- `--delete-catch-up` - set the policy for missed deletions. Accepts `never`, `always`, or a positive duration (e.g., `15m`). Default: `always`. Helm value: `controllerManager.actionTiming.delete.catchUp`.

## Scheduling

### Relative timers

Relative timers (`delete-after`, `restart-after`) start from the selected reference point. By default, `applyTimestamp` means the time when the controller processes the annotation, not the time when a client applies the manifest. Cluster load or controller downtime can delay processing.

Re-applying a manifest that restores the `-after` annotation resets the derived `-at` deadline when the controller processes it. This is the default "keep-alive" behavior.

Use `lifecycle.cezary.dev/reference-point: "creationTimestamp"` to calculate the deadline from `metadata.creationTimestamp`. Re-applying `-after` does not reset an existing `-at` deadline with this reference point.

Use `delete-at` or `restart-at` for a fixed timestamp, or `restart-cron` for a recurring calendar schedule. Actions can run after their scheduled time. These annotations do not guarantee exact-time execution or completion.

### Recurring schedules

For `restart-every` and `restart-cron` schedules, the controller uses a top-level `lifecycle.cezary.dev/last-restart-timestamp: "<timestamp>"` annotation as the anchor for calculating the next restart.

- Initialization - if `last-restart-timestamp` is missing, the controller sets it to the current time.
- Reconciliation - the controller calculates the next occurrence from the schedule and timestamp. When due, it updates the Pod template and timestamp, then schedules the next occurrence. Missed occurrences produce only one template update.

## Restart mechanism

Restarts are supported only for native Deployments, StatefulSets, and DaemonSets in the `apps` API group. Deletion remains available for all watched resources. Other resources receive an `UnsupportedRestartKind` warning Event and their restart annotations remain unchanged.

Deployments and StatefulSets or DaemonSets with `RollingUpdate` can replace Pods after a template change. StatefulSets and DaemonSets with `OnDelete` do not automatically replace existing Pods for this change. Paused Deployments and StatefulSet partitions can also prevent or limit Pod replacement. The controller does not wait for a rollout to complete.

- Triggering a restart - the controller writes `lifecycle.cezary.dev/restartedAt: "<timestamp>"` into the resource's `spec.template.metadata.annotations`. The workload controller determines how to apply the changed template.
  - The same pod template mutation also adds `lifecycle.cezary.dev/managed-by: "lifecycle-controller"` to `spec.template.metadata.annotations`.
- Cleanup - the restart patch also removes the one-time `restart-at` annotation or updates recurring schedule state. This acknowledges the template update, not rollout completion.

## Dry-run

Dry-run logs planned actions without changing resources. Enable it globally with the `--dry-run` flag or the `controllerManager.dryRun` Helm value, or per resource with the `lifecycle.cezary.dev/dry-run` annotation.

In dry-run, relative and recurring schedules are logged without saving state or immediately requeueing the same occurrence.

## Precedence

If a resource mixes `restart-*` and `delete-*` action annotations, the controller records a warning Event and takes no action.

Within one action family, the controller processes a non-empty `delete-after` or `restart-after` before the corresponding `-at` annotation:

| Reference point | When both `-after` and `-at` are present |
| --- | --- |
| `applyTimestamp` (default) | Convert `-after` to a new `-at` value, replacing the existing deadline. |
| `creationTimestamp` | Keep the existing `-at` value and remove `-after`. |

After conversion, restart priority is `restart-at`, then `restart-cron`, then `restart-every`. For deletion, the controller acts on `delete-at`.

## Missed actions

Use a catch-up policy to control actions after their scheduled time. The policy applies to deletion, one-time restarts, and recurring restarts. The default is `always` for both actions.

| Policy | Behavior |
| --- | --- |
| `always` | Execute a due action, with no limit on delay. Missed recurring occurrences produce one restart. |
| A duration, such as `15m` | Execute only if a due occurrence is no more than this duration late. The boundary is inclusive. The deadline uses the scheduled time and does not reset on retries. |
| `never` | Execute an occurrence only if this controller process observed or created it before its due time. Skip an overdue occurrence first seen in this process. |

`never` does not disable future actions. The controller keeps future occurrences in process memory. A matching occurrence stays eligible when its timer is due, including retries after API errors. A controller process restart or leader failover loses this memory. An overdue occurrence first seen by the new process is skipped. Queue or API delays within the same process do not expire an occurrence that was observed before its due time. Use a duration policy to limit those delays.

For a recurring schedule, a duration policy checks for a recent due occurrence. For example, a controller that returns at 03:10 after several missed daily 03:00 restarts can request one restart with `catch-up: "15m"`. It does not replay the missed restarts. If no due occurrence is in the window, it skips them and keeps the next scheduled time.

Set separate global defaults in Helm values:

```yaml
controllerManager:
  actionTiming:
    restart:
      catchUp: "15m"
    delete:
      catchUp: "always"
```

Use `lifecycle.cezary.dev/catch-up` to override the default on a resource:

```yaml
metadata:
  annotations:
    lifecycle.cezary.dev/restart-at: "2026-10-10T03:00:00Z"
    lifecycle.cezary.dev/catch-up: "never"
```

If the annotation is absent, the resource uses the global default for its selected action. An invalid global value prevents startup. An invalid annotation records an `InvalidAnnotation` warning Event and blocks the action.

A skipped action records an `ActionSkipped` Event after the controller saves its acknowledgement. The controller removes the selected one-time `delete-at` or `restart-at` annotation; a skipped deletion leaves the resource in place. For a skipped recurring restart, it advances the schedule state without changing the Pod template. The `catch-up` annotation and recurring schedules stay in place. To request a skipped one-time action again, apply a new action annotation.

The policy starts from the computed deadline for relative timers. With `applyTimestamp`, controller downtime before conversion delays the start of the timer. With `creationTimestamp`, the computed deadline can already be overdue when the controller first reads the resource.
