# lifecycle-controller

A Kubernetes controller that schedules resource deletion and Pod template updates through time-based annotations. It supports namespaced and cluster-scoped resources that expose the required API operations.

Use it to schedule resource deletion or request application restarts without a separate CronJob or wrapper object. Pod replacement depends on the workload's update behavior. See [Restart mechanism](#restart-mechanism).

## Quick start

This example uses a Deployment named `example-app` in `default` namespace, without existing lifecycle action annotations. Actions are limited to Deployments in `default` namespace.

```sh
helm repo add lifecycle-controller https://cstanislawski.github.io/lifecycle-controller
helm repo update
helm install lifecycle-controller lifecycle-controller/lifecycle-controller \
  --namespace lifecycle-controller \
  --create-namespace \
  --set 'controllerManager.scope.watchResources[0]=deployments.apps' \
  --set 'controllerManager.scope.watchNamespaces[0]=default'
kubectl rollout status deployment/lifecycle-controller -n lifecycle-controller
```

Preview a restart after one minute:

```sh
kubectl annotate deployment example-app -n default \
  lifecycle.cezary.dev/dry-run="true" \
  lifecycle.cezary.dev/restart-after="1m" --overwrite
kubectl logs deployment/lifecycle-controller -n lifecycle-controller --since=5m
kubectl get events -n default \
  --field-selector involvedObject.kind=Deployment,involvedObject.name=example-app \
  --sort-by=.lastTimestamp
```

Check for the planned `restart-after` conversion in the logs and a `DryRunRestart` Event. Enable the action:

```sh
kubectl annotate deployment example-app -n default \
  lifecycle.cezary.dev/dry-run="false" --overwrite
```

The controller then converts `restart-after` to `restart-at`. After the scheduled restart time, check for a `RestartTriggered` Event and the Pod template's `lifecycle.cezary.dev/restartedAt` annotation. Use `kubectl rollout status deployment/example-app -n default` to check the workload after the template update.

## Example use cases

### Temporary development environments

To delete feature-branch resources after three days, apply a `delete-after` annotation to each resource.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: feature-branch-x-backend
  namespace: dev-features
  annotations:
    lifecycle.cezary.dev/delete-after: "3d" # Deletes this deployment after 3 days
spec:
  # ...
```

### Nightly application restarts

To request a restart each day at 03:00 in a local timezone, use `restart-cron` and `cron-timezone`.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: legacy-app
  namespace: production
  annotations:
    lifecycle.cezary.dev/restart-cron: "0 3 * * *" # Daily at 3:00 AM
    lifecycle.cezary.dev/cron-timezone: "America/New_York"
spec:
  # ...
```

### One-off scheduled maintenance

Use `restart-at` for a fixed restart time. For a restart after a database migration, use a workflow that waits for migration completion before applying the annotation.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: application
  namespace: production
  annotations:
    lifecycle.cezary.dev/restart-at: "2026-10-10T03:00:00Z"
spec:
  # ...
```

## Annotation API reference

- For all watched resources:
  - `lifecycle.cezary.dev/reference-point` (string) - specifies the starting point for relative duration timers (for `-after` annotations).
    - `applyTimestamp` (default) - the timer starts when the controller processes the `-after` annotation. Re-applying the manifest resets the timer ("keep-alive" behavior).
    - `creationTimestamp` - the timer starts from the resource's creation time. This creates a fixed TTL that is not affected by subsequent updates.
  - `lifecycle.cezary.dev/delete-at` - absolute TTL. The controller deletes the resource at or after this specific date and time (e.g., `2024-12-31T23:59:59Z`).
    - The value must be an RFC3339 timestamp with explicit timezone offset (`Z` or `±hh:mm`).
    - This can be applied directly to a `Namespace` to trigger its deletion. Kubernetes will handle the subsequent removal of all resources within that namespace.
  - `lifecycle.cezary.dev/delete-after` - relative TTL (e.g., `5m`, `1h`, `3d`). The controller calculates a deletion time from the selected reference point, saves it as `lifecycle.cezary.dev/delete-at`, and removes `delete-after`.
  - `lifecycle.cezary.dev/dry-run` - enables dry-run for a resource. Accepts standard boolean values such as `true`, `false`, `1`, and `0`. Invalid values create a warning Event and no action is taken.
  - `lifecycle.cezary.dev/managed-by: "lifecycle-controller"` - added by the controller when it mutates a resource, such as converting `*-after` annotations to `*-at` annotations, maintaining restart schedule state, or triggering a restart.

- For watched resources with a `spec.template` field:
  - `lifecycle.cezary.dev/restart-at` - requests one Pod template update at or after a specific date and time.
    - The value must be an RFC3339 timestamp with explicit timezone offset (`Z` or `±hh:mm`).
  - `lifecycle.cezary.dev/restart-after` - requests one Pod template update after a relative duration (e.g., `1h`). The controller converts this to an absolute `restart-at` annotation.
  - `lifecycle.cezary.dev/restart-every` - requests recurring Pod template updates at a relative interval (e.g., `7d` for weekly updates), with a minimum duration of `1m`.
  - `lifecycle.cezary.dev/restart-cron` - requests recurring Pod template updates based on a standard five-field, minute-resolution cron expression (e.g., `"0 3 * * *"` for daily at 03:00).
  - `lifecycle.cezary.dev/cron-timezone` - optional timezone for `restart-cron` only. Must be valid IANA timezone (e.g., `America/New_York`). Defaults to `UTC`.
  - The controller checks for `spec.template`. Template annotations do not need to exist before the restart request. This field check does not guarantee that the API accepts the patch or that the workload replaces existing Pods.

### Relative duration format

`delete-after`, `restart-after`, and `restart-every` require exactly one positive integer and unit, without signs or spaces. Valid units are `s`, `m`, `h`, and `d`. Values such as `1d`, `25h`, and `90m` are valid.

Zero, compound values (e.g., `1d2h`), arithmetic expressions (e.g., `1d-25h`), decimal values (e.g., `1.5h`), and values that exceed the Go duration limit are invalid. If the selected action has an invalid duration, the controller records an `InvalidAnnotation` warning Event and leaves the resource unchanged.

## Controller behavior

### Relative timers

Relative timers (`delete-after`, `restart-after`) start from the selected reference point. By default, `applyTimestamp` means the time when the controller processes the annotation, not the time when a client applies the manifest. Cluster load or controller downtime can delay processing.

Re-applying a manifest that restores the `-after` annotation resets the derived `-at` deadline when the controller processes it. This is the default "keep-alive" behavior.

Use `lifecycle.cezary.dev/reference-point: "creationTimestamp"` to calculate the deadline from `metadata.creationTimestamp`. Re-applying `-after` does not reset an existing `-at` deadline with this reference point.

Use `delete-at` or `restart-at` for a fixed timestamp, or `restart-cron` for a recurring calendar schedule. Actions can run after their scheduled time. These annotations do not guarantee exact-time execution or completion.

### Precedence

If a resource mixes `restart-*` and `delete-*` action annotations, the controller records a warning Event and takes no action.

Within one action family, the controller processes a non-empty `delete-after` or `restart-after` before the corresponding `-at` annotation:

| Reference point | When both `-after` and `-at` are present |
| --- | --- |
| `applyTimestamp` (default) | Convert `-after` to a new `-at` value, replacing the existing deadline. |
| `creationTimestamp` | Keep the existing `-at` value and remove `-after`. |

After conversion, restart priority is `restart-at`, then `restart-cron`, then `restart-every`. For deletion, the controller acts on `delete-at`.

### Restart mechanism

Deployments and StatefulSets or DaemonSets with `RollingUpdate` can replace Pods after a template change. StatefulSets and DaemonSets with `OnDelete`, and ReplicaSets, do not automatically replace existing Pods for this change. Paused Deployments and StatefulSet partitions can also prevent or limit Pod replacement. The controller does not wait for a rollout to complete.

- Triggering a restart - the controller writes `lifecycle.cezary.dev/restartedAt: "<timestamp>"` into the resource's `spec.template.metadata.annotations`. The workload controller determines how to apply the changed template.
  - The same pod template mutation also adds `lifecycle.cezary.dev/managed-by: "lifecycle-controller"` to `spec.template.metadata.annotations`.
- State tracking for recurring restarts - for `restart-every` and `restart-cron` schedules, the controller uses a top-level `lifecycle.cezary.dev/last-restart-timestamp: "<timestamp>"` annotation as the anchor for calculating the next restart.
  - Initialization - if `last-restart-timestamp` is missing, the controller sets it to the current time.
  - Reconciliation - the controller calculates the next occurrence from the schedule and timestamp. When due, it updates the Pod template and timestamp, then schedules the next occurrence. Missed occurrences produce only one template update.
- Cleanup - the restart patch also removes the one-time `restart-at` annotation or updates recurring schedule state. This acknowledges the template update, not rollout completion.

### Dry-run

Dry-run logs planned actions without changing resources. Enable it globally with the `--dry-run` flag or the `controllerManager.dryRun` Helm value, or per resource with the `lifecycle.cezary.dev/dry-run` annotation.

In dry-run, relative and recurring schedules are logged without saving state or immediately requeueing the same occurrence.

## Scope and permissions

### Configuration flags

By default, the controller discovers resources across all namespaces and watches those that support `get`, `list`, `watch`, `patch`, and `delete`. It skips subresources and APIs that lack these verbs. An explicit watch pattern that selects an unsupported API causes a discovery error unless an ignore rule excludes that API.

Resource filters select which API types to watch. The controller reads, deletes, and restarts resources only in the filtered namespaces. Namespace patterns limit deletions and restarts but require cluster-wide reads.

- `--watch-resource` (repeatable) - glob pattern for resources to watch.
  - Format - `<resource>.<group>` for grouped APIs (e.g., `deployments.apps`) or `<resource>` for core APIs (e.g., `pods`).
  - If not provided, all eligible resources are watched unless excluded by ignore rules. Literal `*.*` is not equivalent to this default: it does not match core keys such as `pods`.
  - Broad patterns can select APIs that lack the required verbs. Prefer exact resource names.
- `--ignore-resource` (repeatable) - glob pattern for resources to strictly ignore. Takes precedence over watch rules.
- `--watch-namespace` (repeatable) - glob pattern for namespaces to watch (e.g. `default`, `dev-*`).
  - If provided, the controller deletes or restarts only objects in matching namespaces. Namespace objects also require `--watch-resource=namespaces`. Other cluster-scoped resources are excluded.
- `--ignore-namespace` (repeatable) - glob pattern for namespaces to strictly ignore. Takes precedence over watch rules.

### RBAC

By default, the Helm chart watches standard workloads, ConfigMaps, Services, Ingresses, NetworkPolicies, PersistentVolumeClaims, HorizontalPodAutoscalers, PodDisruptionBudgets, Namespaces, and PersistentVolumes. It grants `get`, `list`, `watch`, `patch`, `update`, and `delete` for those resources, plus `Event` permissions and permissions for enabled leader-election and secure-metrics features.

A list of filtered namespaces creates a `Role` and `RoleBinding` in each watched namespace. Namespace patterns or leaving the list unset results in a `ClusterRole`. Namespace objects still require cluster permissions.

For resource patterns such as `deploy*.apps`, set `rbac.create: false` and supply your own RBAC with explicit resource names.
