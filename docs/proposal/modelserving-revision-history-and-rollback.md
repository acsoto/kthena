---
title: ModelServing Revision History and Rollback
authors:
- "@acsoto"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-08-13

---

## ModelServing Revision History and Rollback

### Summary

This proposal makes ModelServing revisions stable, retained, and usable for
rollback. It aligns the revision lifecycle with Kubernetes StatefulSet and the
general RBG model: historical workload definition is versioned, while the
current controller and runtime context is used to construct the desired
workload.

The central contract is:

> A `ControllerRevision` stores the versioned workload declaration that should
> be restored by rollback. It does not snapshot every input that can affect the
> final rendered Pod.

Revision identity is the versioned user workload declaration. A desired
workload is produced by combining that declaration with the current rendering
context. Rollback restores the declaration only. It does not promise bit-for-
bit or field-for-field reproduction of the Pod that existed when the revision
was created.

### Motivation

ModelServing already creates `ControllerRevision` objects, but the revision
hash and stored data are built from different inputs. Operational changes such
as Role scaling can therefore keep the same hash while changing the stored
data. In addition, revision numbers do not form a history and completed
revisions are removed.

These behaviors prevent reliable recovery of an earlier desired workload. The
design also needs a clear boundary so that every new controller or plugin input
does not silently become historical revision data.

#### Goals

- Define revision identity as a small, explicit workload declaration.
- Build deterministic revision data and its identity from the same allowlisted
  projection.
- Keep revision data immutable, reuse equivalent revisions, and make revision
  numbers monotonic.
- Retain recent history without deleting revisions used by live workloads.
- Restore a historical workload definition through the existing rollout
  strategies.
- Preserve current capacity, scheduling, ownership, and rollout policies during
  rollback.
- Define plugin rendering dependencies without snapshotting arbitrary live
  ModelServing state or plugin implementation details.

#### Non-Goals

- Bit-for-bit reproduction of a historical Pod.
- A revision of every input that can affect final Pod rendering.
- Snapshotting `OwnerReferences`, plugin executable code, or implementation
  versions in `ControllerRevision.Data`.
- A separate rollback state machine.
- Rollback of an individual Role.
- Guaranteed CLI rollback to legacy revisions.
- Runtime implementation changes in this documentation PR; those are reviewed
  separately in #1767.

### Proposal

ModelServing will record the versioned workload declaration before scale and
rollout reconciliation. This also records template changes when `replicas` is
zero.

The semantic model is:

```text
revision
    = versioned user workload declaration

desired workload
    = historical/current revisioned workload definition
    + current rendering context
```

The resulting flow is:

```text
ControllerRevision
    |
    | historical workload declaration
    v
Roles / templates / workerReplicas / plugins
    |
    + current scheduler / current owner context / current controller behavior
    v
desired workload
    |
    v
compare with existing workload
    |
    v
replace or update when effective rendering differs
```

The current rendering context may include the current scheduler configuration,
the current owning LWS identity when a plugin needs it, Pod and Role identity,
and the behavior of the current controller and plugin implementations. These
inputs are deliberately not historical revision data.

The CLI will expose revision history and rollback:

```bash
kthena rollout history modelserving <name>
kthena rollout undo modelserving <name> --to-revision=<revision>
```

Omitting `--to-revision`, or setting it to zero, selects the previous revision.
The CLI reads the selected `ControllerRevision` and applies its revisioned
fields to `ModelServing.spec`. The controller then handles the resulting change
as a normal `ServingGroupRollingUpdate` or `RoleRollingUpdate`.

Both rollout strategies must replace a workload when a change to its effective
rendered workload requires replacement. The comparison is not required to use
exactly the same inputs as `ControllerRevision.Data`, because current rendering
inputs intentionally live outside the revision. The concrete comparison or
hashing mechanism remains an implementation detail of each strategy.

The CLI retries conflicts by reading the latest ModelServing and reapplying the
revisioned fields, so concurrent changes to operational fields are not
overwritten.

### Design Details

#### Revision Data

`ControllerRevision.Data` will contain a deterministic strategic merge patch
with only explicitly selected workload-definition fields. `BuildRevisionData`
constructs this projection from an allowlist; it must not copy the API object
and remove known operational fields. API defaults and nil/empty values are
normalized before serialization.

The revisioned workload inputs are:

```text
- Role identity and membership
- Role entry template
- Role worker template
- workerReplicas
- plugins
```

`plugins` remains revisioned for now. The entire `PluginSpec` participates in
the revision identity:

```text
- name
- type
- config
- scope
- plugin list order
```

This is required because plugin configuration is explicitly supplied by the
user as part of the workload declaration, `OnPodCreate` plugins may mutate the
Pod, and plugin order is semantically significant: a later plugin can observe
or overwrite an earlier mutation. The current plugin API does not declare
which plugins or configuration fields affect Pod rendering, so this proposal
does not introduce plugin-specific revision metadata.

Canonicalization is still required:

- Opaque JSON `config` is canonicalized before hashing and serialization.
- `scope.roles` is canonicalized because role-list order has no semantic
  meaning.
- Plugin list order is preserved because execution order is significant.
- Roles are stored in canonical name order, so reordering otherwise identical
  Roles does not create a revision.

For `RoleRollingUpdate`, only plugins applicable to the specific Role
participate in that Role's workload identity. A plugin scoped only to `prefill`
must not make `decode` outdated. A plugin with no role restriction applies to
each applicable Role, and the relative order of the applicable plugins remains
the order in the ModelServing plugin list.

Revision membership uses replace semantics so applying a revision can remove
Roles and plugins added after that revision. `ApplyRevision` preserves the
operational Role properties described below while restoring the plugin chain
represented by the revision. The serialized patch is used unchanged as
`ControllerRevision.Data.Raw` and as the primary revision identity input.

`ControllerRevision.Data` is immutable after creation. The revision identity is
used in the ControllerRevision name, ModelServing status, and workload labels.
Changes to current rendering inputs do not create a new ControllerRevision;
they are handled by effective-rendering comparison and reconciliation.

#### Current Rendering Context and Plugin Contract

`schedulerName` is a current rendering input, not a revisioned field. The
controller uses the current scheduler configuration when constructing Pods.
Changing `schedulerName` must still make workloads whose final Pod
`schedulerName` changes outdated and cause replacement.

For example:

```text
revision A:
  image = v1

revision B:
  image = v2

current schedulerName:
  new-scheduler

rollback B -> A
```

The resulting workload is:

```text
image = v1
schedulerName = new-scheduler
```

It must not restore the scheduler that happened to be configured when revision
A was created.

The current plugin API exposes the whole ModelServing object through
`HookRequest.ModelServing`. That shape is an implementation compatibility
detail, not a promise that arbitrary live ModelServing fields are part of the
historical rendering contract. The intended semantic rule is:

> `OnPodCreate` workload mutation should depend only on the plugin
> declaration/configuration, Pod and Role identity, and explicitly supported
> current rendering context.

Plugins must not rely on arbitrary live fields such as:

```text
- ModelServing status
- rolloutStrategy
- replicas
- unrelated Roles
- controller revision bookkeeping labels
- PodGroup operational annotations
- arbitrary external mutable state
```

If a plugin requires runtime context, that context must be explicitly defined
and supplied as current rendering context. A synthetic partial ModelServing
object may be used temporarily for compatibility, but this proposal defines
the semantic dependency contract rather than standardizing that workaround as
the long-term API model.

OwnerReferences are live object-relationship metadata, not part of the
historical workload declaration. They must not be stored in revision data or
included in the revision identity. In particular, do not add a historical
owner-reference field or store historical APIVersion, Kind, Name, UID,
`controller`, or `blockOwnerDeletion` values for plugin rendering. The
`ControllerRevision` object's own live metadata may still use the ownership
needed for its lifecycle; that metadata is not revision data and must not be
passed to a plugin as historical context.

This matters for the built-in `lws-standard-labels` plugin. It derives labels
from the current owning LeaderWorkerSet. If revision A was originally owned by
LWS `foo`, but the current ModelServing is later owned by LWS `bar`, recovery
of revision A must use the current LWS identity:

```text
LWS name = bar
```

It must not add historical labels referring to `foo`. If the implementation
needs to expose this identity to the plugin, it should pass only the minimum
current rendering context required, such as the current LeaderWorkerSet name.

ControllerRevision stores plugin declaration and configuration, not plugin
executable code. Built-in plugin implementations should preserve rendering
compatibility for previously supported configuration semantics. If a plugin
intentionally introduces incompatible rendering behavior, it should use an
explicit configuration or behavior version. This proposal does not snapshot
plugin implementation versions or add a new versioning subsystem.

#### Revisioned, Rendering, and Operational Inputs

The proposal distinguishes three categories:

| Category | Examples | Stored in ControllerRevision | Restored by rollback | Can trigger workload replacement |
| --- | --- | --- | --- | --- |
| Revisioned workload inputs | Role identity and membership, Role entry and worker templates, `workerReplicas`, applicable `PluginSpec` chain | Yes | Yes | Yes |
| Current rendering inputs | `schedulerName`, current owning LWS identity, explicitly supported current rendering context | No | No | Yes, when the final Pod/workload changes |
| Operational/control inputs | ModelServing/Role `replicas`, `rolloutStrategy`, `partition`, `maxUnavailable`, `maxSurge`, `recoveryPolicy`, `restartGracePeriodSeconds`, `gangPolicy`, `networkTopology` | No | No | Only when their semantics directly require it; normally they control reconciliation rather than workload identity |

The controller should reason about the effective workload as:

```text
EffectiveRendering =
    RevisionedWorkloadInputs
  + CurrentRenderingInputs
```

A workload is outdated when its effective rendered workload differs from the
currently desired effective rendered workload. This rule intentionally differs
from requiring outdated-workload detection to use exactly the same fields as
revision data: `schedulerName` and current LWS identity can be outside the
revision while still requiring replacement when they change the rendered
workload. The exact comparison or hashing implementation is implementation
specific.

Operational fields control capacity, rollout progression, recovery, or
scheduling policy. They are preserved when rollback changes the workload
version and normally affect reconciliation rather than revision identity.

#### Applying a Revision

Applying revision R restores only the revisioned workload definition and
preserves current non-revisioned values.

Preserve:

```text
- ModelServing replicas
- Role replicas
- schedulerName
- rolloutStrategy
- maxUnavailable
- maxSurge
- partition
- recoveryPolicy
- restartGracePeriodSeconds
- gangPolicy
- networkTopology
- live OwnerReferences
```

Restore:

```text
- Role membership and identity
- Role entry and worker Pod templates
- workerReplicas
- plugins
```

For a Role present in both the current spec and revision R, its current
operational replica count and list position are preserved. A Role restored from
history that is absent from the current spec may use the existing documented
default replica behavior (currently `1`) and is appended in canonical name
order after retained Roles. A current Role absent from R is removed.

The resulting spec must still satisfy current API validation. In particular,
the current operational `gangPolicy` can prevent removal of a Role referenced
by `minRoleReplicas`; the CLI rejects such a rollback without changing the
ModelServing.

Rollback restores only the workload definition; all non-revisioned fields stay
current. The controller then renders the restored definition using the current
context.

#### Effective Rendering and Outdated Workloads

The controller must apply the effective-rendering rule in both
`ServingGroupRollingUpdate` and `RoleRollingUpdate`:

1. Select the revisioned workload definition for the workload being created or
   recovered.
2. Combine it with current rendering inputs.
3. Construct the desired effective workload using the current controller and
   plugin implementations.
4. Compare the desired effective workload with the existing workload.
5. Replace or update the workload when the rollout strategy requires it.

For `RoleRollingUpdate`, the per-Role comparison must use that Role's
revisioned definition and only its applicable plugin chain. A change to a
plugin scoped only to `prefill` must not make a `decode` workload outdated.
Operational fields remain available to the rollout and reconciliation logic,
but do not become revision identity merely because they influence capacity or
progression.

The contract is about the effective workload selected by the current
implementation. A plugin hook is not required to reproduce the output of an
older controller or plugin implementation, and an old Pod is not guaranteed to
be reproduced after controller, plugin, scheduler integration, owner context,
or other runtime behavior changes.

#### Rollback and Recovery

Historical recovery restores the workload definition represented by revision R
and then renders it under the current context:

```text
recover revision R
    ↓
load revisioned Role/plugin definition from R
    ↓
combine with current scheduler/runtime/owner context
    ↓
construct desired Pod or workload
```

A recovered historical Role can therefore use the current scheduler and can
receive labels derived from the current LWS owner. That is expected behavior.
Historical scheduler or owner metadata is not required for recovery and must
not be treated as part of the recovery contract.

Revision references required for partition-protected workloads must survive
Pod deletion and controller restart. The controller must be able to determine
which historical workload revision a protected workload should use even when
the Pod and in-memory store are gone. After that revision is selected, the
replacement workload is rendered with the current rendering context; this
requirement does not depend on historical scheduler or owner metadata.

#### Revision Lifecycle

For each desired workload, the controller builds a candidate revision from the
revisioned workload inputs only:

```text
nextRevision = max(history.Revision) + 1
```

It then follows the StatefulSet revision lifecycle:

1. If the candidate equals the latest revision, reuse the latest revision.
2. If it equals an older revision, reuse that ControllerRevision and advance
   its `Revision` to `nextRevision`.
3. Otherwise, create a new ControllerRevision.

Only the numeric `Revision` is updated when an older revision is reused; its
name and immutable Data remain unchanged. For example:

```text
A(revision=1) -> B(revision=2) -> C(revision=3)
rollback to A
A's ControllerRevision is reused with revision=4
```

Changes only to current rendering inputs do not create a revision. They can
still make effective workloads outdated and require replacement.

Hash collisions are handled with a collision count stored in
`ModelServingStatus`:

```go
CollisionCount *int32 `json:"collisionCount,omitempty"`
```

A name collision with different Data increments the collision count and salts
the hash calculation. Existing Data is never overwritten.

#### History Retention

`ModelServingSpec` will add:

```go
// +kubebuilder:default=10
// +kubebuilder:validation:Minimum=0
RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
```

The default is `10`. Zero retains no non-live history, and negative values are
rejected.

The limit applies to non-live history. Revisions referenced by
`CurrentRevision`, `UpdateRevision`, or existing child workloads are live and
do not count toward the limit. Non-live revisions are deleted from oldest to
newest until at most `revisionHistoryLimit` remain.

The revision references required for partition-protected recovery remain live
until the replacement is created. Garbage collection therefore cannot rely
only on existing Pod labels or the controller's in-memory store.

#### Compatibility

New revisions will carry the annotation
`modelserving.volcano.sh/revision-data-version: "v1"` so they can be
distinguished from the legacy wrapped Role list.

- Legacy revisions remain readable for active rollout and recovery.
- The first reconciliation with the v1 format builds revision data and a hash
  through the normal revision lifecycle. Because this identity differs from a
  legacy revision, upgrading the controller initiates a one-time rollout of an
  otherwise unchanged ModelServing.
- The migration does not persist a compatibility baseline or maintain an alias
  between legacy and v1 revisions. The one-time rollout follows the configured
  rollout strategy, `maxUnavailable`, and `partition`.
- CLI history and rollback only guarantee revisions in the new format.
- Legacy revision Data is not rewritten or renumbered and is removed through
  normal history retention once it is no longer live.
- Built-in plugins should preserve the rendering semantics of previously
  supported configurations; intentionally incompatible behavior requires an
  explicit plugin configuration or behavior version.

#### Test Plan

The implementation will add unit and end-to-end coverage for:

- deterministic allowlist projection, canonical JSON configuration, role-scope
  normalization, and preserved plugin order;
- equivalent revision reuse, immutable Data, monotonic numbers, collision
  handling, retention, live-revision protection, and legacy compatibility;
- rollback restoring Role/plugin definitions while preserving all current
  operational fields, including `schedulerName`, `maxSurge`, and live owner
  relationships;
- scheduler changes causing replacement without creating historical scheduler
  data;
- RoleRollingUpdate filtering plugin identity by Role applicability;
- protected recovery after Pod deletion and controller restart using the
  selected historical revision with current scheduler and owner context;
- LWS label rendering using the current owning LWS rather than a historical
  owner; and
- plugin implementation compatibility expectations for previously supported
  configuration semantics.

### Alternatives

#### Snapshot every rendering input

Including scheduler settings, OwnerReferences, controller bookkeeping, and all
live ModelServing fields in every revision could make a historical Pod appear
more reproducible. It would also couple revision history to operational state,
arbitrary plugin reads, and implementation details that cannot be reliably
restored. It expands the revision every time a controller integration reads a
new field and still cannot snapshot executable plugin behavior. This proposal
keeps the revision at the workload declaration boundary instead.

#### Append-only rollback history

Creating another ControllerRevision whenever old content becomes desired would
preserve every transition as a separate object. This was rejected because it
duplicates revision data and diverges from the StatefulSet lifecycle. Reusing
an equivalent revision also preserves the existing `<name>-<hash>` identity.

#### Controller-managed rollback state

Adding a rollback request to the API would require separate rollback state and
completion handling. Updating the desired spec through the CLI keeps rollback
declarative and reuses the existing rollout implementation.

#### Zero-rollout legacy migration

Keeping the legacy revision active while recording equivalent v1 data would
require a persistent compatibility baseline or alias and special reconciliation
and GC rules. This was rejected in favor of a one-time rollout through the
normal revision lifecycle.

### Scope

This PR changes only this design proposal. Runtime changes are intentionally
out of scope and should be reviewed separately against the simplified
revision/current-rendering contract.
