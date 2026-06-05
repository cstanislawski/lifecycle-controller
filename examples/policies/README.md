# Admission Policy Examples

`lifecycle-controller` treats top-level `metadata.annotations` with the `lifecycle.cezary.dev/` prefix as operational intent. Those annotations can schedule deletes, including `Namespace` deletes, and trigger rolling restarts. In shared clusters, restrict who can create, change, or remove them.

These examples are opt-in starting points:

- `kyverno-restrict-lifecycle-annotations.yaml`: Kyverno `ClusterPolicy`.
- `gatekeeper-restrict-lifecycle-annotations.yaml`: Gatekeeper `ConstraintTemplate` and `Constraint`.

Before applying either example, edit the allowed principals for your install:

- Helm default: `system:serviceaccount:lifecycle-controller:lifecycle-controller`
- kustomize default: `system:serviceaccount:lifecycle-controller:lifecycle-controller-controller-manager`
- admin group example: `lifecycle-controller-annotation-admins`

The examples intentionally protect only top-level lifecycle annotations. The controller's rollout marker under `spec.template.metadata.annotations` is not guarded here because it is not an input the controller turns into a privileged action.

These policies use AdmissionReview fields such as user and operation, so audit/background scans cannot fully evaluate the authorization logic. Use admission enforcement for the safety boundary.
