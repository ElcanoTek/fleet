# ADR-0055: On the kubernetes backend the skills tree is staged into the workspace claim

- **Status:** Accepted
- **Date:** 2026-09-02
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0049](0049-kubernetes-backend-split-control-plane.md) — its
  "no control-plane push into the workspace claim" non-goal, for the **skills
  root only**; every other doc root keeps ADR-0049's bake-and-declare posture.
- **Relates to:** [ADR-0054](0054-agent-plugins.md) (plugin skills live in the
  merged tree), [ADR-0036](0036-sandboxed-file-tools-and-host-io-exceptions.md)
  (the host-side exception list — **unchanged**),
  [docs/SHARED-FILES.md](../SHARED-FILES.md) (the staged-tree pattern this
  reuses)

## Context

The skills tree an agent reads is **assembled at boot from three sources**:
fleet's built-in pack (embedded in the binary), every Agent Plugin's `skills/`
(ADR-0054), and the bundle's own `skills/`. `clientconfig.Load` materializes
them into one merged directory under the control plane's data dir
(`$FLEET_DATA_DIR/skills-merged/<hash of the bundle path>`) and points
`Bundle.SkillsDir` at it; the prompt roster advertises `skills/<name>/SKILL.md`,
and the per-conversation workspace `skills` symlink resolves that to the
merged tree.

Under podman that tree is bind-mounted read-only into every sandbox at the
same absolute path, so the roster path resolves for `view_file`, `bash` and
`run_python` alike. Under the kubernetes backend a sandbox pod mounts **only
the workspace claim**. ADR-0049 answered "how does a pod read bundle docs?"
with *bake the bundle's doc dirs into the sandbox image at the same absolute
paths and declare it* (`bundle_docs_in_image`), and explicitly ruled out the
alternative — a control-plane push into the claim — because it "would put
bundle content on a writable, agent-reachable surface".

That answer cannot cover the skills tree, structurally:

- The merged tree does not exist in the bundle checkout. Its path is derived
  from a runtime hash, it is rebuilt at every boot, and two of its three
  sources — the pack inside the *binary* and plugin overlays — are not files
  an image build of the bundle can `COPY`.
- So a bundle that inherited the pack got a roster of names it could not open:
  `SKILL.md`, reference files and bundled scripts all failed inside a pod.
  The documented remedy was `skills_builtin: false`, which trades away the
  entire built-in pack to make `skills/` a plain, bake-able directory.
- Plugin skills (ADR-0054) made the gap permanent: they *always* land in the
  merged tree, so on this backend they were rostered but unreadable
  regardless of the knob. ADR-0054 recorded that as an accepted cost and
  #1383 deferred it as "an ADR-0049-sized design".

Meanwhile the shared file library shipped the pattern that fits: a read-only
tree **staged under the workspace root** — the one directory both backends
make visible — that the kubernetes pod spec re-mounts from the same claim as
a read-only `subPath`, so every pod reads it and no turn can write it. The
manager already classifies any read-only root nested inside the claim that
way (`splitWorkspaceNestedMounts`), and the file-tool anchor already treats
it as read-only. Nothing about that shape is specific to shared files.

## Decision

1. **On the kubernetes backend, fleet stages the complete skills tree into the
   workspace claim at boot.** Before the sandbox pool is built and before the
   skills dir is registered anywhere, `agent.StageSkillsForBackend` calls
   `Bundle.StageSkillsAt(<workspace root>/skills)`: the same
   `syncMergedSkills` that builds the data-dir tree rebuilds it — built-in
   pack (per `skills_builtin` / `skills_hidden`), plugin overlays, bundle
   `skills/` — at that exact path, and `SkillsDir` points there. `Skills()`
   keeps resyncing the staged tree on every read, so the
   edit-in-place and plugin-folder-follows-the-disk contracts hold in a pod.
2. **The staged tree is a read-only subPath mount in every sandbox pod.** It
   flows through the existing nested-mount rule: a read-only root inside the
   claim is re-mounted from the same claim as `subPath: skills, readOnly:
   true`, and the fileop anchor resolves reads under it as read-only and
   refuses writes. `bundle_docs_in_image` no longer governs skills at all; it
   still governs `protocols/`, `personas/` and `system_prompts/`.
3. **Staging happens on every kubernetes boot, pack or no pack.** Even a
   bundle with `skills_builtin: false` and no plugins is staged: the staged
   copy is what makes `skills/<name>/SKILL.md` resolve in a pod *without*
   baking skills into the sandbox image, and it is the only way plugin skills
   and the pack reach a pod at all. `skills_builtin: false` is once again a
   taste decision, not a kubernetes one.
4. **The staged tree is a derived cache and is never adopted blindly.** The
   bundle checkout stays the source of truth (validation, `SkillOrigin`, the
   library UI's provenance all read the sources). Because the workspace
   volume is writable by sandboxes until the read-only mount exists — and was
   writable at the staged path by every pod that ran before the first boot of
   a fleet carrying this ADR — the staging code answers ADR-0049's objection
   directly rather than waving it off:
   - a pre-existing staged root that is not a plain directory owned by the
     control plane and closed to group/world writes (a planted symlink, a
     `0777` dir) is **removed and rebuilt**, loudly;
   - every file write inside the tree is **symlink- and hard-link-safe**: a
     planted symlink or file where a directory belongs is removed first, and
     file bytes go to a temp file renamed over the target, so nothing is ever
     written *through* an existing entry (a symlink to a control-plane file, a
     hard link to another conversation's file) and a pod never reads a torn
     `SKILL.md`;
   - stale entries are removed on every sync, and the whole tree is rebuilt
     at every boot, so a wiped or tampered volume heals itself.
   The same hardening applies to the data-dir tree, where it is merely
   belt-and-braces.
5. **Podman is unchanged.** Its same-path bind mount of the data-dir tree
   works; staging is skipped, prompt paths stay byte-identical
   ([PROMPT-CACHE-CONTRACT.md](../PROMPT-CACHE-CONTRACT.md)), and a box that
   later flips backends simply starts staging.
6. **Only the skills root is staged.** `protocols/`, `personas/` and
   `system_prompts/` are plain bundle files that an image build *can* carry,
   pinned to a release by the image digest; ADR-0049's bake-and-declare
   posture stands for them. The mechanism here would extend to them
   trivially, and a later ADR may decide it should — this one lifts the
   restriction only where the image route is structurally impossible.

## Enforcement

- `internal/clientconfig/builtin_skills.go` — `StageSkillsAt`,
  `ensureStagedRoot`, the symlink-safe `ensureRealDir` / `writeFileIfChanged`;
  `IsMaterializedSkillsDir` now documents that it is the *degraded-path*
  check (staging failed) rather than the normal kubernetes case.
- `internal/agent/manager.go` — `StageSkillsForBackend` (podman no-op,
  kubernetes stages), called by `cmd/fleet` (`fleet serve`) and
  `internal/taskrun` (`fleet task run`) before `tools.SetSupportingDocDirs`
  and before the pool is built.
- `internal/tools/workspace.go` — `SkillsDirName` / `StagedSkillsDir`, the one
  constant every consumer shares (symlink name, staged path), mirroring
  `SharedFilesDirName`.
- Tests: `clientconfig/staged_skills_test.go` (all three sources staged;
  live reload in the staged tree; the manifest knobs respected; the bundle's
  own dir refused; planted root symlink / world-writable root / inner file
  symlink / hard link / symlink-where-a-dir-belongs each replaced with the
  target untouched; atomic idempotent writes), `agent/stage_skills_test.go`
  (podman untouched, kubernetes staged at `<root>/skills` and classified as a
  nested read-only mount, failure leaves the dir in place),
  `sandbox/k8s_backend_test.go::TestK8sPodSpecStagedSkillsSubPathMount` (the
  pod spec carries the read-only subPath mount and the anchor agrees).

## Consequences

- **Easier:** a kubernetes bundle inherits the built-in pack like every other
  bundle; Agent Plugin skills work inside a pod; the sandbox image no longer
  needs `COPY skills/`, and a skill edit needs no image rebuild — the staged
  tree follows the bundle on the next read, exactly as on podman. The
  example bundle
  ([example-kubernetes-config](https://github.com/ElcanoTek/example-kubernetes-config))
  drops `skills_builtin: false` and ships a plugin.
- **Unchanged:** the security posture. The tree is read-only in every pod,
  the anchor refuses writes, ADR-0036's host-side exception list gains
  nothing, and no credential or connector content is staged (the tree holds
  skill docs and scripts the bundle author already ships to sandboxes).
- **Costs accepted:** the skill bytes exist twice on a cluster (bundle in the
  control-plane image, staged copy in the workspace claim) — the same
  double-storage the shared library accepts, and small. The control plane
  must be able to create `<workspace root>/skills` (already required for
  `shared`); a storage class that hands the control plane a root it cannot
  write degrades loudly to the pre-ADR posture (skills rostered, unreadable
  in pods), never to a boot failure. A turn that ran under an older fleet
  could have planted content at the staged path; the first boot of this
  fleet replaces it, which is the intended behaviour and is logged.
- **ADR-0049's non-goal is narrowed, not reversed:** fleet still does not
  project bundle content into the claim in general; it stages the one tree
  the image route cannot carry, read-only, with the tampering objection
  answered in code and pinned by tests.

## Alternatives considered

- **Keep `skills_builtin: false` as the answer** (the pre-ADR posture).
  Rejected: it costs the entire pack and does nothing for plugin skills,
  which always live in the merged tree — the deployment shape the platform
  calls first-class would permanently lack a feature the podman shape has.
- **Bake the merged tree into the sandbox image.** Impossible in the general
  case: the tree is built at boot at a hash-derived path from sources that
  include the running binary's embedded pack and whatever plugins the
  bundle loads; an image build cannot reproduce it and would freeze it.
- **Project the tree with a ConfigMap.** Rejected: ConfigMaps cap at 1 MiB
  (the bento-slides skill alone exceeds that), would need a per-boot object
  the sandbox RBAC grant does not cover, and put the skills on a surface the
  fleet process must be allowed to write cluster-wide.
- **An init container that copies skills from the control-plane image into
  each pod.** Rejected: it moves the pack and plugins into the sandbox image
  or a sidecar image the operator must build, costs a copy per pod start
  (warm pools included), and still cannot see plugins added to the bundle
  without an image rebuild.
- **Stage every doc root** (protocols, personas, system prompts too) and
  retire `bundle_docs_in_image`. Not chosen here: those roots are bake-able
  and their image snapshot pins them to a release, which some deployments
  want; the mechanism is ready if a later ADR decides otherwise.
