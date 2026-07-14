# lifecycle-controller

A Kubernetes controller to manage the lifecycle of any Kubernetes resource, namespaced or not, through simple, time-based annotations.

This controller allows you to automate common operational tasks such as cleaning up temporary resources or scheduling periodic application restarts without needing to create complex CronJobs or custom wrapper objects.

## Example Use Cases

Here are a few scenarios where `lifecycle-controller` can simplify your workflow.

### Temporary Development Environments

Scenario: A developer spins up resources for a feature branch. To prevent cluttering the cluster, these resources should be automatically deleted after 3 days.

Solution: Apply a `delete-after` annotation to the resources.

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

### Nightly Application Restarts

Scenario: A legacy application has a slow memory leak. To ensure stability, the operations team wants to restart it every night at 3:00 AM in their local timezone.

Solution: Use the `restart-cron` and `cron-timezone` annotations.

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

### One-Off Scheduled Maintenance

Scenario: A database migration is scheduled for Saturday at 2:00 AM UTC. The application pods need to be restarted immediately after to pick up the new schema. This can be scheduled in advance.

Solution: Use the `restart-at` annotation with a specific timestamp.

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: database
  namespace: production
  annotations:
    lifecycle.cezary.dev/restart-at: "2025-10-25T02:00:00Z"
spec:
  # ...
```

## Annotation API Reference

The controller's behavior is configured entirely through annotations.

- **For all resources:**
  - `lifecycle.cezary.dev/reference-point`: (string) Specifies the starting point for relative duration timers (for `-after` annotations).
    - `applyTimestamp` (default): The timer starts when the controller processes the `-after` annotation. Re-applying the manifest resets the timer ("keep-alive" behavior).
    - `creationTimestamp`: The timer starts from the resource's creation time. This creates a fixed TTL that is not affected by subsequent updates.
  - `lifecycle.cezary.dev/delete-at`: Absolute TTL. The controller deletes the resource at or after this specific date and time (e.g., `2024-12-31T23:59:59Z`).
    - The value must be an RFC3339 timestamp with explicit timezone offset (`Z` or `±hh:mm`).
    - This can be applied directly to a `Namespace` to trigger its deletion. Kubernetes will handle the subsequent removal of all resources within that namespace.
  - `lifecycle.cezary.dev/delete-after`: Relative TTL (e.g., `5m`, `1h`, `3d`). The controller processes this annotation by calculating an absolute deletion time based on the time it first notices the annotation. It then adds a `lifecycle.cezary.dev/delete-at` annotation to the resource with this calculated time. To prevent re-calculation and make the state explicit, the original `lifecycle.cezary.dev/delete-after` annotation is then removed. **Supports `s`, `m`, `h`, and `d` (days).**
  - `lifecycle.cezary.dev/dry-run: "true"`: Per-resource dry-run annotation. The controller logs actions it would take without executing them.
  - `lifecycle.cezary.dev/managed-by: "lifecycle-controller"`: Added by the controller when it mutates a resource, such as converting `*-after` annotations to `*-at` annotations, maintaining restart schedule state, or triggering a restart.

- **Only for pod-spawning resources (Deployments, StatefulSets, DaemonSets, etc.):**
  - `lifecycle.cezary.dev/restart-at`: Performs a one-time rolling restart at a specific date and time.
    - The value must be an RFC3339 timestamp with explicit timezone offset (`Z` or `±hh:mm`).
  - `lifecycle.cezary.dev/restart-after`: Performs a one-time rolling restart after a relative duration (e.g., `1h`). The controller converts this to an absolute `restart-at` annotation. Supports `s`, `m`, `h`, and `d` (days).
  - `lifecycle.cezary.dev/restart-every`: Performs a rolling restart on a recurring, relative basis (e.g., `7d` to restart weekly). Supports `s`, `m`, `h`, and `d` (days).
  - `lifecycle.cezary.dev/restart-cron`: Performs a rolling restart based on a cron expression (e.g., `"0 3 * * *"` for daily at 3 AM).
  - `lifecycle.cezary.dev/cron-timezone`: Optional timezone for `restart-cron` only. Must be valid IANA timezone (e.g., `America/New_York`). Defaults to `UTC`.
  - A resource is considered pod-spawning if it has a `spec.template.metadata.annotations` field. The restart mechanism works by patching this field, which is the standard Kubernetes pattern for triggering a rolling update.

- **Restart Mechanism**
  - **Triggering a Restart** - To initiate a rolling restart, the controller injects a `lifecycle.cezary.dev/restartedAt: "<timestamp>"` annotation into the resource's`spec.template.metadata.annotations`. This is the standard mechanism that causes Kubernetes to detect a change in the pod template and trigger a rollout.
    - The same pod template mutation also adds `lifecycle.cezary.dev/managed-by: "lifecycle-controller"` to `spec.template.metadata.annotations`.
  - **State Tracking for Recurring Restarts** - For `restart-every` and `restart-cron` schedules, the controller maintains its state using a top-level `lifecycle.cezary.dev/last-restart-timestamp: "<timestamp>"` annotation on the resource. This timestamp serves as the anchor for calculating the next restart, ensuring the schedule remains stable over time.
    - **Initialization** - If the `last-restart-timestamp` annotation is missing on a resource with a recurring restart schedule, the controller adds it and sets its value to the current time. This bootstraps the schedule.
    - **Reconciliation Logic** - On each check, the controller performs the following steps:
      - Reads the schedule (`restart-every` or `restart-cron`) and the `last-restart-timestamp`,
      - Calculates the `nextScheduledRestart` time based on the last one,
      - Compares the current time to the `nextScheduledRestart` time,
      - If the current time is at or after `nextScheduledRestart`, the controller triggers the restart,
      - It then updates the `lifecycle.cezary.dev/last-restart-timestamp` to the value of `nextScheduledRestart`. This anchors the next cycle to the previous scheduled time, preventing schedule drift.
  - **Cleanup** - After a one-time `restart-at` action is successfully triggered, the controller will remove the original `lifecycle.cezary.dev/restart-at` annotation to ensure the action is idempotent.

**Dry-run**
Dry-run logs the actions the controller would take without executing them. It can be enabled globally (`--dry-run=true|false` or Helm `controllerManager.dryRun: true`) or per-resource via `lifecycle.cezary.dev/dry-run: "true"`. Dry-run is enabled for a given resource if either is set.

- **Precedence:**
  - **Conflicting action types** - If a resource mixes annotations from different action families (any combination of `restart-*` and `delete-*`), the controller treats it as a misconfiguration. It will post a warning `Event` on the resource and take no action.
  - **Multiple annotations of one family** - If more than one annotation of the same family is present on a resource, the controller applies the most specific one ("most specific wins").
  - **In case of restarts**
    - `restart-after` is a convenience annotation that is converted into `restart-at` by the controller.
    - `restart-at` (a specific, one-time event) takes highest priority.
    - `restart-cron` (a specific, recurring schedule) is next.
    - `restart-every` (a relative interval) has the lowest priority.
  - **In case of deletes**
    - `delete-after` is a convenience annotation that is converted into `delete-at` by the controller.
    - `delete-at` (a specific, one-time event) takes highest priority.

### A Note on Relative Timers

Annotations that use relative durations (`delete-after`, `restart-after`) start their from a configurable reference point. While the controller's processing of annotations is usually immediate, factors like high cluster load or controller downtime can introduce delays.

By default, the timer starts from the `applyTimestamp`. This means the timer begins when the controller processes the annotation on the resource.

Crucially, the timer will be reset every time you re-apply the manifest containing the `-after` annotation. The controller treats each `apply` as a new declaration of intent and recalculates the absolute `-at` timestamp based on the current time. This "keep-alive" behavior is the default and is very useful for development environments where resources should persist as long as they are actively being worked on.

To set a fixed lifetime that is not reset on subsequent updates, you can change the reference point. By adding the annotation `lifecycle.cezary.dev/reference-point: "creationTimestamp"`, you instruct the controller to start the timer from the moment the resource was created (`metadata.creationTimestamp`). This creates a strict, fixed TTL that is ideal for CI preview environments or automated test resources that must be cleaned up after a set period, regardless of any updates.

For time-critical operations or to set a fixed expiration that does not change on subsequent applies, it is recommended to use the absolute time annotations (`delete-at`, `restart-at`, `restart-cron`). These define a specific, unambiguous point in time for the action to occur, making them more reliable for scheduled maintenance or cleanup.

## Scope & Permissions

By default, the controller discovers and watches **all** available resources (`*.*`) across **all** namespaces. In production or multi-tenant environments, you may want to restrict this scope to improve security and performance.

The controller supports glob-style patterns for filtering resources and namespaces via command-line flags (or Helm values).

### Configuration Flags

- `--watch-resource` - (Repeatable) Glob pattern for resources to watch.
  - Format: `<resource>.<group>` (e.g. `deployments.apps`, `pods`, `*.k8s.io`).
  - If not provided, all resources are watched (unless excluded by ignore rules).
- `--ignore-resource` - (Repeatable) Glob pattern for resources to strictly ignore. Takes precedence over watch rules.
- `--watch-namespace` - (Repeatable) Glob pattern for namespaces to watch (e.g. `default`, `dev-*`).
  - Strict Scoping: If provided, the controller will **only** watch resources inside matching namespaces. It will **automatically exclude** cluster-scoped resources (like `Nodes`) with the exception of `Namespace` objects themselves, provided their name matches the pattern.
- `--ignore-namespace` - (Repeatable) Glob pattern for namespaces to strictly ignore. Takes precedence over watch rules.

### Examples

Watch only deployments in `dev-*` namespaces:

```bash
--watch-resource=deployments.apps --watch-namespace=dev-*
```

Watch everything except secrets and anything in `kube-system`:

```bash
--ignore-resource=secrets --ignore-namespace=kube-system
```

## Deployment

`lifecycle-controller` is both as:

- A standalone container that you can configure to run in your own environment
- A Helm chart for easy deployment into a Kubernetes cluster

### A Note on Permissions (RBAC)

By default, the controller generates a `ClusterRole` with wildcard (`*`) permissions for `resources` and `apiGroups`. This allows it to dynamically discover and manage any resource type.

When installing via Helm, if you configure the `scope.watchResources` value, the chart will automatically restrict the generated `ClusterRole` to only contain permissions for those specific resources. This ensures the controller operates with the Principle of Least Privilege.

If you are running the binary manually or managing RBAC yourself, you should ensure your `ClusterRole` permissions match the resources you intend to manage.

### Installation with Helm

You can install the controller directly from the Helm repository:

```sh
helm repo add lifecycle-controller https://cstanislawski.github.io/lifecycle-controller
helm repo update
helm install lifecycle-controller lifecycle-controller/lifecycle-controller \
  --namespace lifecycle-controller \
  --create-namespace
```

To configure scoping via Helm (which also tightens RBAC):

```yaml
controllerManager:
  scope:
    watchResources:
      - "deployments.apps"
      - "statefulsets.apps"
    watchNamespaces:
      - "default"
      - "dev-*"
```
