# lifecycle-controller

A Kubernetes controller to manage resources through time-based actions.

## Features

The core functionality, focusing on direct, annotation-driven actions.

- **For all resources:**
  - `lifecycle.io/timezone`: The timezone to use for all time-based annotations on the resource. Default is `UTC`. This timezone is used to interpret all datetime strings and cron expressions for this resource. In case an invalid timezone is provided, the controller will post a warning Event on the resource, and take no action.
  - `lifecycle.io/delete-at`: Absolute TTL. The controller deletes the resource at or after this specific date and time (e.g., `2024-12-31T23:59:59`).
    - The value should be an ISO 8601 format timestamp. The timezone is determined by the `timezone` annotation.
    - This can be applied directly to a `Namespace` to trigger its deletion. Kubernetes will handle the subsequent removal of all resources within that namespace.
  - `lifecycle.io/delete-after`: Relative TTL (e.g., `5m`, `1h`, `30d`). The controller processes this annotation by calculating the absolute deletion time and adding a `lifecycle.io/delete-at` annotation to the resource. To prevent re-calculation and make the state explicit, the original `lifecycle.io/delete-after` annotation is then removed.
  - `lifecycle.io/dry-run: "true"`: A per-resource annotation that makes the controller log the actions it *would* take without executing them. This can also be set via a global flag on the controller.

- **Only for pod-spawning resources (Deployments, StatefulSets, etc.):**
  - `lifecycle.io/restart-at`: Performs a one-time rolling restart at a specific date and time.
  - `lifecycle.io/restart-every`: Performs a rolling restart on a recurring, relative basis (e.g., `7d` to restart weekly).
  - `lifecycle.io/restart-cron`: Performs a rolling restart based on a cron expression (e.g., `"0 3 * * *"` for daily at 3 AM).
  - A resource is considered pod-spawning if it has a `spec.template.metadata.annotations` field, which is a pattern that tools like `kustomize`, or controllers like `Argo Rollouts` use to determine if the resource is a pod-spawning resource.

- **Restart Mechanism:**
  - **Triggering a Restart** - To initiate a rolling restart, the controller injects a `lifecycle.io/restartedAt: "<timestamp>"` annotation into the resource's`spec.template.metadata.annotations`. This is the standard mechanism that causes Kubernetes to detect a change in the pod template and trigger a rollout.
  - **State Tracking for Recurring Restarts** - For `restart-every` and `restart-cron` schedules, the controller maintains its state using a top-level `lifecycle.io/last-restart-timestamp: "<timestamp>"` annotation on the resource. This timestamp serves as the anchor for calculating the next restart, ensuring the schedule remains stable over time.
    - **Initialization** - If the `last-restart-timestamp` annotation is missing on a resource with a recurring restart schedule, the controller adds it and sets its value to the current time. This bootstraps the schedule.
    - **Reconciliation Logic** - On each check, the controller performs the following steps:
      1. Reads the schedule (`restart-every` or `restart-cron`) and the `last-restart-timestamp`,
      2. Calculates the `nextScheduledRestart` time based on the last one,
      3. Compares the current time to the `nextScheduledRestart` time,
      4. If the current time is at or after `nextScheduledRestart`, the controller triggers the restart,
      5. It then updates the `lifecycle.io/last-restart-timestamp` to the value of `nextScheduledRestart`. This anchors the next cycle to the previous scheduled time, preventing schedule drift.

  - **Cleanup** - After a one-time `restart-at` action is successfully triggered, the controller will remove the original `lifecycle.io/restart-at` annotation to ensure the action is idempotent.

- **Note on Precedence:**
  - **Conflicting action types** - If a resource mixes annotations from different action families (any combination of `restart-*` and `delete-*`), the controller treats it as a misconfiguration. It will post a warning `Event` on the resource and take no action.
  - **Multiple annotations of one family** - If more than one annotation of the same family is present on a resource, the controller applies the most specific one ("most specific wins").
  - **In case of restarts:**
    - `restart-at` (a specific, one-time event) takes highest priority.
    - `restart-cron` (a specific, recurring schedule) is next.
    - `restart-every` (a relative interval) has the lowest priority.
  - **In case of deletes:**
    - `delete-at` (a specific, one-time event) takes highest priority.
    - `delete-after` (a relative interval) has the lowest priority.
