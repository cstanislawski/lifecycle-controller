# lifecycle-controller

A Kubernetes controller to manage the lifecycle of standard Kubernetes resources (like Deployments, StatefulSets, and Namespaces) through simple, time-based annotations.

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

Solution: Use the `restart-cron` and `timezone` annotations.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: legacy-app
  namespace: production
  annotations:
    lifecycle.cezary.dev/restart-cron: "0 3 * * *" # Daily at 3:00 AM
    lifecycle.cezary.dev/timezone: "America/New_York"
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
  - `lifecycle.cezary.dev/timezone`: The timezone to use for all time-based annotations on the resource. Default is `UTC`. This timezone is used to interpret all datetime strings and cron expressions for this resource. In case an invalid timezone is provided, the controller will post a warning Event on the resource, and take no action.
  - `lifecycle.cezary.dev/reference-point`: (string) Specifies the starting point for relative duration timers (for `-after` annotations).
    - `applyTimestamp` (default): The timer starts when the controller processes the `-after` annotation. Re-applying the manifest resets the timer ("keep-alive" behavior).
    - `creationTimestamp`: The timer starts from the resource's creation time. This creates a fixed TTL that is not affected by subsequent updates.
  - `lifecycle.cezary.dev/delete-at`: Absolute TTL. The controller deletes the resource at or after this specific date and time (e.g., `2024-12-31T23:59:59`).
    - The value should be an ISO 8601 format timestamp. The timezone is determined by the `timezone` annotation.
    - This can be applied directly to a `Namespace` to trigger its deletion. Kubernetes will handle the subsequent removal of all resources within that namespace.
  - `lifecycle.cezary.dev/delete-after`: Relative TTL (e.g., `5m`, `1h`, `3d`). The controller processes this annotation by calculating an absolute deletion time based on the time it first notices the annotation. It then adds a `lifecycle.cezary.dev/delete-at` annotation to the resource with this calculated time. To prevent re-calculation and make the state explicit, the original `lifecycle.cezary.dev/delete-after` annotation is then removed. **Supports `s`, `m`, `h`, and `d` (days).**
  - `lifecycle.cezary.dev/dry-run: "true"`: A per-resource annotation that makes the controller log the actions it *would* take without executing them. This can also be set via a global flag on the controller.

- **Only for pod-spawning resources (Deployments, StatefulSets, etc.):**
  - `lifecycle.cezary.dev/restart-at`: Performs a one-time rolling restart at a specific date and time.
  - `lifecycle.cezary.dev/restart-after`: Performs a one-time rolling restart after a relative duration (e.g., `1h`). The controller converts this to an absolute `restart-at` annotation. Supports `s`, `m`, `h`, and `d` (days).
  - `lifecycle.cezary.dev/restart-every`: Performs a rolling restart on a recurring, relative basis (e.g., `7d` to restart weekly). Supports `s`, `m`, `h`, and `d` (days).
  - `lifecycle.cezary.dev/restart-cron`: Performs a rolling restart based on a cron expression (e.g., `"0 3 * * *"` for daily at 3 AM).
  - A resource is considered pod-spawning if it has a `spec.template.metadata.annotations` field, which is a pattern that tools like `kustomize`, or controllers like `Argo Rollouts` use to determine if the resource is a pod-spawning resource.

- **Restart Mechanism**
  - **Triggering a Restart** - To initiate a rolling restart, the controller injects a `lifecycle.cezary.dev/restartedAt: "<timestamp>"` annotation into the resource's`spec.template.metadata.annotations`. This is the standard mechanism that causes Kubernetes to detect a change in the pod template and trigger a rollout.
  - **State Tracking for Recurring Restarts** - For `restart-every` and `restart-cron` schedules, the controller maintains its state using a top-level `lifecycle.cezary.dev/last-restart-timestamp: "<timestamp>"` annotation on the resource. This timestamp serves as the anchor for calculating the next restart, ensuring the schedule remains stable over time.
    - **Initialization** - If the `last-restart-timestamp` annotation is missing on a resource with a recurring restart schedule, the controller adds it and sets its value to the current time. This bootstraps the schedule.
    - **Reconciliation Logic** - On each check, the controller performs the following steps:
      - Reads the schedule (`restart-every` or `restart-cron`) and the `last-restart-timestamp`,
      - Calculates the `nextScheduledRestart` time based on the last one,
      - Compares the current time to the `nextScheduledRestart` time,
      - If the current time is at or after `nextScheduledRestart`, the controller triggers the restart,
      - It then updates the `lifecycle.cezary.dev/last-restart-timestamp` to the value of `nextScheduledRestart`. This anchors the next cycle to the previous scheduled time, preventing schedule drift.
  - **Cleanup** - After a one-time `restart-at` action is successfully triggered, the controller will remove the original `lifecycle.cezary.dev/restart-at` annotation to ensure the action is idempotent.

- **Note on Precedence:**
  - **Conflicting action types** - If a resource mixes annotations from different action families (any combination of `restart-*` and `delete-*`), the controller treats it as a misconfiguration. It will post a warning `Event` on the resource and take no action.
  - **Multiple annotations of one family** - If more than one annotation of the same family is present on a resource, the controller applies the most specific one ("most specific wins").
  - **In case of restarts:**
    - `restart-after` is a convenience annotation that is converted into `restart-at` by the controller.
    - `restart-at` (a specific, one-time event) takes highest priority.
    - `restart-cron` (a specific, recurring schedule) is next.
    - `restart-every` (a relative interval) has the lowest priority.
  - **In case of deletes:**
    - `delete-after` is a convenience annotation that is converted into `delete-at` by the controller.
    - `delete-at` (a specific, one-time event) takes highest priority.

### A Note on Relative Timers

Annotations that use relative durations (`delete-after`, `restart-after`, `restart-every`) start their from a configurable reference point. While the controller's processing of annotations is usually immediate, factors like high cluster load or controller downtime can introduce delays.

By default, the timer starts from the `applyTimestamp`. This means the timer begins when the controller processes the annotation on the resource.

Crucially, the timer will be reset every time you re-apply the manifest containing the `-after` annotation. The controller treats each `apply` as a new declaration of intent and recalculates the absolute `-at` timestamp based on the current time. This "keep-alive" behavior is the default and is very useful for development environments where resources should persist as long as they are actively being worked on.

To set a fixed lifetime that is not reset on subsequent updates, you can change the reference point. By adding the annotation `lifecycle.cezary.dev/reference-point: "creationTimestamp"`, you instruct the controller to start the timer from the moment the resource was created (`metadata.creationTimestamp`). This creates a strict, fixed TTL that is ideal for CI preview environments or automated test resources that must be cleaned up after a set period, regardless of any updates.

For time-critical operations or to set a fixed expiration that does not change on subsequent applies, it is recommended to use the absolute time annotations (`delete-at`, `restart-at`, `restart-cron`). These define a specific, unambiguous point in time for the action to occur, making them more reliable for scheduled maintenance or cleanup.

## Deployment

`lifecycle-controller` is both as:

- A standalone container that you can configure to run in your own environment
- A Helm chart for easy deployment into a Kubernetes cluster

### Installation with Helm

You can install the controller directly from the Helm repository:

```sh
helm repo add lifecycle-controller https://cstanislawski.github.io/lifecycle-controller
helm repo update
helm install lifecycle-controller lifecycle-controller/lifecycle-controller \
  --namespace lifecycle-controller-system \
  --create-namespace
```

or with a custom values file:

```sh
helm install lifecycle-controller ./charts/lifecycle-controller \
  --namespace lifecycle-controller-system \
  --create-namespace \
  -f custom.yaml
```
