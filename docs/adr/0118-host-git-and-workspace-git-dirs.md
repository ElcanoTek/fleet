# ADR-0118: The host runs no git in the workspace repository; its git directories are read-only in whole-root sandboxes

- **Status:** Accepted
- **Date:** 2026-11-19
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0058](0058-per-conversation-attachment-scoping.md) (the
  whole-root workspace mount it describes) and
  [ADR-0097](0097-scheduled-sandbox-mounts-only-its-job-directory.md) (the
  read-only set of whole-root sandboxes, and the worktree-run take)
- **Relates to:** [ADR-0036](0036-sandboxed-file-tools-and-host-io-exceptions.md)
  (host-side operations; the no-follow opener for the workspace tree),
  [ADR-0069](0069-per-job-scheduled-workspaces.md)

## Context

fleet used to run git on the host, as the fleet user, for its own
bookkeeping:

- `internal/scheduledrun` created the per-run worktree and branch of every
  `worktree_config` task in the repository at the workspace root, and
  removed them after the run;
- `internal/worktree` (the hourly maintenance sweep) and `fleet worktree
  list|prune` listed and reclaimed those worktrees;
- the operator CLI reads the fleet checkout and the client-bundle checkout
  (`fleet update --check`, which also fetches, and the bundle residue note).

The workspace root is the directory every whole-root sandbox — every chat
turn, approved bash, worktree runs and shared-workspace scheduled runs —
mounts read-write (ADR-0058, ADR-0097). When it is a git repository, its
`.git` is written from those sandboxes, and a worktree run's sandbox writes
it by design: its commits land in the root repository.

Git reads more than data from a repository. Hook scripts, hooks defined in
configuration, `core.fsmonitor`, content filters selected through
attributes, transports and their helpers name programs, and git runs them.
Configuration arrives through `include.path` and `includeIf` conditions that
hold only in a worktree's own git process. Even git that runs no program
reads whatever the repository's files lead to: refs, pack indexes and
worktree records a live run can replace with FIFOs (git blocks reading them)
or links to `/dev/zero` (git reads without end), a record's `commondir`
naming any directory, `.git/worktrees` itself a link out of the workspace.
`git worktree remove` and `git worktree prune` delete whatever the records
lead to. And the root may not have been a repository when fleet started: a
sandbox can `git init` it.

`tasks/` (ADR-0097) and `mcp-runs/` already get a read-only overlay in
whole-root sandboxes for the analogous reason: the host acts on them by
pathname. `mcp-shared/`, the shared MCP spawns' `${FLEET_WORKSPACE}`, holds
connector ledgers and reports written the same way and had no overlay.

## Decision

**1. The host runs no git in the workspace repository.** Registering a
worktree, listing, removing and reclaiming worktrees, and deleting a run's
branch are done by `internal/worktree` without git (decisions 2–4); resolving
the base, creating the branch and filling the checkout happen in the run's
sandbox (decision 2). The only host git left is in the operator's own
checkouts, through `internal/hostgit`: the fleet checkout and the
client-bundle checkout, which nothing fleet runs can write while they lie
outside the workspace root. There hostgit
keeps the operator's credential helpers, ssh command and transports, turns
off hooks (`core.hooksPath=/dev/null` and `hook.<event>.enabled=false` for
every event), fsmonitor, editors, pagers, submodule recursion and automatic
maintenance, accepts only the subcommands those checks use, bounds every
command (`MaxDuration`, 2 minutes, whatever deadline the caller passes, and a
`WaitDelay` on the pipes), and clears the variables that would point git at
another repository (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_COMMON_DIR`, …). It does
not itself check where the checkout it is given lies: the fleet and
client-bundle checkouts are directories the operator configured, and keeping
them out of the workspace root is the operator's to do (see the remaining
gaps). A guard test (constants resolved with go/types) fails on any other
process start that runs git — directly, as a `git-*` helper, behind a
wrapper such as `env` or `timeout`, or in a shell command line — and on any
process start whose program it cannot resolve, or that passes a program to
a function that starts it, unless an allowlist entry gives the reason.

**2. fleet writes the worktree's record itself; the run's sandbox makes the
branch and the checkout.** `worktree.CreateWorktree` writes, through
directories opened without following a link — `.git/worktrees` and
`.fleet-worktrees`, both read-only to every sandbox — what
`git worktree add --no-checkout` would: a `locked` file first, then `gitdir`
(the checkout's `.git`), `commondir` (`../..`) and `HEAD`
(`ref: refs/heads/<branch>`, unborn), then the checkout's `.git` file
(`gitdir: <record>`), and removes `locked` once the record is complete, as
git does (a lock left behind would make the sweep keep the worktree for
ever). Paths are written with the workspace root as configured, which is the
path sandboxes mount it at. A name already taken (record or checkout
directory) fails the call and is left as it was; on any failure the call
removes only what it created, and the run's setup then removes nothing. The
run's first worker pass runs, in its sandbox and in the worktree
(`scheduledrun.checkoutInSandbox`):

```sh
# the sandbox's git must be 2.23 or later (git version, checked first)
git -c safe.directory='*' -c core.hooksPath=/dev/null rev-parse --quiet --verify '<base>^{commit}'
git -c safe.directory='*' -c core.hooksPath=/dev/null switch --quiet --no-track --create '<branch>' '<base>'
```

The base (`main-worktree/HEAD` by default, the task's `base_branch`
otherwise; a `base_branch` of `HEAD`, `@` or a pseudo-ref such as
`FETCH_HEAD` means the repository's and is read through `main-worktree/`,
since inside the new worktree those name the run's own) is resolved there, and the branch, its reflog, the index and the
files are written there. `safe.directory` covers a sandbox user whose uid
differs from the files' owner (a pod on the kubernetes backend). The run's
cleanup deletes the branch only when this checkout created it.

- `--create`, not `--force-create`: an existing branch of the run's name
  fails the run, naming it, and is never reset.
- `--no-track`: a remote-tracking base (`origin/main`) would otherwise make
  git record the new branch's upstream in `.git/config`, which is
  read-only there.
- `core.hooksPath=/dev/null` applies to this one command: the repository's
  hooks (a git-lfs `post-checkout`, say) do not run during fleet's checkout,
  so one that fails, or needs a tool the image lacks, cannot fail the run
  before it starts. The run's own git commands run the hooks as usual.
- A base that does not name a commit fails the run there with the base
  named; with the default base that means the repository has no commits, and
  the message says so.
- The sandbox image needs git 2.23 or later (`git switch`; the
  `main-worktree/` ref prefix arrived in 2.20). The version is checked
  first, so an older git fails the run naming its version rather than
  reading as an empty repository.

**3. Listing, removing and reclaiming worktrees, and deleting a run's
branch, run no git.** `internal/worktree` works through `os.Root` handles
opened with the workspace tree's no-follow opener (`tools.OpenWorkspaceDir`,
ADR-0036). Entries that are links are skipped. A record counts as a live
worktree's only when the checkout's own `.git` file and the record's
`gitdir` name each other (a run can rewrite only its own pair), and a lock is
honoured only in such a record — except the `initializing` lock
`CreateWorktree` removes when it finishes, which counts for nothing on a
checkout past the prune age. A record whose `gitdir` names a
`.fleet-worktrees` directory that is gone is stale, and so is one holding
nothing but an `initializing` lock older than the prune age (a server
stopped before it wrote `gitdir`); reading such a record lists at most two
of its entries, since a run can fill its own record with any number. A
run's cleanup removes its own record by the id its setup gave it, without
proof, once `RepoPin.Check` passes — unless the run locked its worktree
(`git worktree lock`): then the checkout, the record and the branch all
stay, as `git worktree remove` and the sweep leave a locked worktree, until
the operator unlocks and prunes it. The run's branch is deleted, only when
its own sandbox created it, by unlinking `refs/heads/<branch>` and `logs/refs/heads/<branch>`
relative to directories opened without following a link, and by rewriting
`packed-refs` (which no sandbox can write) after taking `packed-refs.lock`;
the new file keeps the old one's permissions and is synced before it
replaces it.
Every host read of a file a sandbox can write (a record's `gitdir` and
`locked`, a checkout's `.git`, `packed-refs`, `info/exclude`) Lstats the
entry (a link or anything but a regular file is refused — `os.Root` would
otherwise follow a link that stays inside its directory), opens it with
`O_NOFOLLOW|O_NONBLOCK` (a FIFO swapped in fails at once), checks that the
opened file is the one Lstat'ed (`os.SameFile`), and reads a bounded size.

**4. fleet acts only on the `.git` the workspace root had at startup.** At
boot the pool records whether the root's `.git` is a real directory, and
which one (`worktree.PinAtStartup`); nothing records a root later. A
one-off `fleet worktree prune|list` or `fleet doctor` is a separate process
and records whatever `.git` it finds at its own start — including one a
chat sandbox created after a server booted without one, which the server
itself leaves alone. What such a command does there stays inside
`.git/worktrees` and `.fleet-worktrees`, through no-follow handles, under
the same checks. Before acting, host code checks that it still is that directory and
points nowhere else (`RepoPin.Check`): no `commondir`; real `refs/`,
`worktrees/`, `logs/`, `info/`, `lfs/`, `objects/` and `objects/info/`
directories reached without following a link; no `objects/info/alternates`; regular
`HEAD`, `config` and `packed-refs` files; no reftable ref storage. Anything
it cannot establish is a refusal. A root not recorded, one that had no `.git`
at startup, whose `.git` was replaced since, whose `.git` is a gitfile or a
symlink, or that uses reftable, gets no worktree tasks (a clear error) and no
record pruning (reported once per process). The directories `Check` requires
that git does not always create (`worktrees/`, `logs/refs/heads/`,
`objects/info/`, `info/`, `lfs/`) are created at boot, in mock mode too, so
the mounts in decision 5 exist; `TakeWorktree` refuses a run without
`objects`, `objects/info`, `refs`, `logs` or `lfs`. Sandboxes a crashed prior process left
running keep that process's mounts (a writable `.git` on the first start
after an upgrade), so on the podman backend they are reclaimed before the
repository is recorded and prepared. `fleet doctor` and the server's boot
log run the same check (`boxdoctor.CheckWorktreeGit`) and name the root they
checked (the CLI, which does not carry the service's environment, takes
`FLEET_WORKSPACE_ROOT` from its own environment or the server env file, else
`./workspace`, relative to the service home `/var/lib/fleet` when that
exists, as do `fleet worktree list|prune` without `--workspace`); the doctor's "ok" says what it found now (the server decides from
its own startup record, so a `.git` created later takes a restart), and a
root the doctor cannot look into (not root, `/var/lib/fleet` 0700) is
skipped, not reported as refused.

**5. In whole-root sandboxes, the root repository's `.git` and
`.fleet-worktrees/` are their own read-only bind mounts.** When the root's
`.git` is a real directory at boot, the pool records it and `.fleet-worktrees`
(created then) in `ContainerConfig.WholeRootGitDirs`. Every whole-root
sandbox — the warm pool, chat turns, approved bash, shared-workspace
scheduled runs — mounts both read-only, as mount points none can rename or
replace. A worktree run takes `Pool.TakeWorktree` (always a cold start),
which adds, writable and nested in them, exactly what its git writes: its
checkout (`.fleet-worktrees/<run>`), its record (`.git/worktrees/<id>`:
index, HEAD, its reflog), and the object store, refs, reflogs and Git LFS
store (`.git/objects`, `.git/refs`, `.git/logs`, `.git/lfs`), with
`.git/objects/info` read-only again inside the object store. The host
creates `.git/lfs` at boot and never reads or acts on anything under it. Config, hooks, `info/`, `HEAD`, `packed-refs`,
`commondir`, other worktrees' records and checkouts stay read-only. Podman
adds a `--volume` per mount, parents first; kubernetes a `subPath` mount of
the claim for each that exists, so the kubelet never creates one.

**6. `mcp-shared/` joins `tasks/` in the whole-root read-only set,** created
at boot (a symlink on the name fails boot, as for `mcp-runs/`). Scoped
sandboxes still do not mount it.

**7. `.fleet-worktrees` must be a real directory** before a worktree is
registered in it; `.git/info/exclude` is appended to through the opener and
the regular-file check of decision 3.

## Alternatives considered

- **Run `git worktree add` on the host with every program switched off.**
  The previous form of this decision. Switching off programs leaves what git
  reads: a live run's FIFO ref, pack index or record file blocks it until the
  caller's deadline, a ref linked to `/dev/zero` makes it read without end,
  and a record's `commondir` chooses which files it reads. Not running git
  removes all of it.
- **Check the ref path, then let host git write the run's branch.** A live
  worktree run can swap a directory under `.git/refs` or `.git/logs` for a
  symlink between the check and git's write.
- **`git worktree remove` / `git worktree prune`.** They delete through
  symlinks under `.git/worktrees`.
- **Mount `.git/reftable` writable for worktree runs.** Possible, but reftable
  rewrites `tables.list` and compacts tables in place, and nothing here has
  been exercised against it; refusing is the conservative choice until it is.
- **Decide the git mounts per sandbox start, with a post-start identity
  check.** Deciding at boot gives the property `tasks/` has: once mounted,
  nobody can swap it.
- **Scope worktree runs to their worktree plus `.git`.** Narrower still, but a
  change to what worktree runs can see of the rest of the root (ADR-0097 left
  it open), not needed for this decision.

## Consequences

- A per-run worktree is checked out by the sandbox's git, with the sandbox
  image's configuration: an operator's host-wide git-lfs (or other filter)
  no longer fills worktrees; a filter the run needs must be available in the
  sandbox image.
- Worktree tasks need the workspace root's `.git` to be a real directory when
  fleet starts. A root made a repository later is refused until a restart; a
  root that is itself a linked worktree (a gitfile), or that uses reftable
  ref storage, is not supported. The host's git version no longer matters to
  worktree tasks (the sandbox image's does).
- A worktree run's branch exists only once its sandbox has started and
  created it; a run that fails before that leaves no branch.
- Repository hooks do not run during fleet's checkout of a run's worktree
  (they do for the run's own git commands). A git-lfs repository's worktree
  holds LFS pointer files until the run fetches the objects itself
  (`git lfs pull`), which works when git-lfs is in the sandbox image:
  `.git/lfs` is writable in worktree runs (and read-only in every other
  whole-root sandbox). The default image does not include git-lfs.
- Submodules cannot be initialised in a worktree run: `git submodule update
  --init` writes `.git/config` and `.git/modules`, which are read-only there.
- A branch a run creates from a remote-tracking ref needs `--no-track`, and
  `git push -u` / `--set-upstream` cannot record an upstream: both write
  `.git/config`. The run's task prompt says so.
- Worktree runs share the repository's `.git/objects`, `.git/refs` and
  `.git/logs`. The ownership proof of decision 3 holds per record and per
  checkout, not for refs: a run can move or delete `main`, another run's
  branch or any ref, and that changes what the next run's base resolves to.
  Runs of different tasks against one repository trust each other with its
  refs, as they did when all of them committed to it directly.
- In a worktree run's sandbox, commits, creating branches, fetches into
  `refs/remotes` and pushes work, but after most ref updates (`switch`,
  every `commit`) git prints
  `error: Unable to create '…/packed-refs.lock': Read-only file system` and
  still exits 0. Deleting any branch, `git branch -m`, `git gc` /
  `git pack-refs`, `git config` writes (`branch --set-upstream-to`) and
  changes to another worktree fail. The run's task prompt says so
  (scheduledrun.withWorktreeNotice); no configuration keeps git from trying
  the packed-refs lock, and the mount stays read-only.
- A whole-root chat sandbox can read the root repository, but git commands
  that write to it (commit, stash, switch, branch) fail there with
  "Read-only file system"; the root's tracked files themselves are as
  writable as the rest of the root. A persistent conversation sandbox does
  not mount the root at all. The user guide says so, and the chat's
  working-directory prompt mentions the root repository only when the root
  was recorded as one at startup and every turn's sandbox mounts the whole
  root. Repositories in a conversation's own directory are unaffected.
- Record pruning considers only fleet's records (gitdir under
  `.fleet-worktrees`); anything else in `.git/worktrees` is left for the
  operator's own `git worktree prune`.
- `fleet update --check` keeps working with the operator's credential helpers
  and any transport the checkout's remote uses.
- Worktree runs always cold-start their sandbox. They are unattended, like
  the scoped runs of ADR-0097.
- Operator-authored host commands — a task's `run_if` gate, the scripts
  `fleet update` and `fleet doctor` run — are outside hostgit by design; they
  run what the operator wrote, and an operator who runs git in the workspace
  repository by hand runs it with whatever that repository holds.
- Remaining gaps, stated plainly:
  - On the kubernetes backend, pods a crashed prior control plane left
    running are reclaimed only after the repository has been recorded and
    prepared (the pod client exists only then); during that window they keep
    their old mounts.
  - Inside one record or checkout directory, intermediate names are
    resolved by `os.Root`, which follows a link that stays inside the
    directory it was opened on; only the final name is Lstat'ed and
    compared. A run that links inside its own record makes fleet read its
    own other files there, nothing outside.
  - A run's sandbox can make its own branch or checkout unusable, or delete
    refs others rely on (see above); fleet keeps the host's actions inside
    the workspace and bounded in time, not the runs' work safe from each
    other.
  - Operator commands do not check whether a working directory or an
    operator checkout lies inside the workspace root: hostgit runs git in
    whatever checkout the operator configured, the CLI takes scripts and
    unit files from `./scripts` and `./deploy` of the working directory, and
    env files and the default bundle under it are read as found. Keeping the
    fleet checkout, the client bundle and the working directory of a root
    `fleet` command out of the workspace root is the operator's to do; a
    separate change adds those checks.
