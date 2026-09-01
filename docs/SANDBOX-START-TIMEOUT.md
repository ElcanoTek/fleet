# Sandbox start timeout & the keep-id image pre-warm (#1358)

What shipped for issue #1358, and how to read the symptom it fixes.

## The symptom

On a box with slow I/O (WSL2 very much included), the **first sandbox start
after a new sandbox image lands** used to fail — every time, on boot's
warm-pool fill, on `cmd/sandbox-probe`, and on every turn's cold start — with
the opaque error:

```
take sandbox: podman run: signal: killed (stderr: )
```

while a plain `podman run <image> echo ok` finished in ~2s, which sent
operators hunting for a podman regression that wasn't there.

## The root cause

fleet starts every sandbox with `--userns=keep-id:uid=1000,gid=1000`. The
**first** keep-id run of an image makes podman build an **id-remapped copy of
every image layer** into the rootless store — a one-time cost per
(image, idmap) that measured 88s wall / 75s CPU for the multi-GB sandbox image
on WSL2. `NewContainer` bounded `podman run` with a hard-coded 30s start
timeout, so the context expiry SIGKILLed podman mid-copy ("signal: killed",
empty stderr), and each retry restarted the copy from scratch. Once the copy
completes it is cached and every start takes ~2s — which is why one manual
keep-id `podman run` "fixed" a wedged box.

Podman single-box deployments hit this on **every sandbox image update**; the
kubernetes backend is unaffected (pods have their own 2-minute start ceiling
and no keep-id copy).

## What shipped

1. **Boot-time pre-warm** (`internal/sandbox/prewarm.go`). Before the warm
   pool spawns its first container, boot runs one throwaway
   `podman run --rm --userns=keep-id:uid=1000,gid=1000 <image> true` under a
   generous 15-minute budget, with an honest log line ("… can take minutes on
   slow disks"). A marker file under the data dir
   (`data/sandbox-keepid-prewarm`) records the local image ID last warmed, so
   an unchanged image costs one `podman image inspect` (~100ms) per boot.
   `fleet update` restarts the service, so a pulled-new image is warmed by the
   next boot before anything else can trip over it. The pre-warm is
   best-effort: a failure logs and continues, because the tunable timeout
   below remains the backstop and a genuinely broken podman still surfaces
   through the pool.

2. **A tunable start timeout** — `FLEET_SANDBOX_START_TIMEOUT_SECONDS`
   (typed knob, min 1; unset keeps each backend's default). It bounds one
   sandbox start: the `podman run` that creates a container (default 30s), or
   one pod's schedule+pull+start under `FLEET_SANDBOX_BACKEND=kubernetes`
   (default 2 minutes). This is the escape hatch for boxes where even
   *prepared* starts are slow; the 30s default is deliberately kept as a
   real-hang detector.

3. **The failure names itself.** When `podman run` dies to the start-timeout
   context, the error now says the configured timeout, the knob to raise, and
   the id-remap cause — instead of the bare `signal: killed`.

## Troubleshooting translation

| You see | It means | Do |
| --- | --- | --- |
| `podman run: sandbox start exceeded the 30s start timeout …` | A start ran past `FLEET_SANDBOX_START_TIMEOUT_SECONDS` (or its default). If the sandbox image just changed and boot's pre-warm didn't run (e.g. the image was re-pulled under the same tag mid-flight, or the pre-warm itself failed — check the journal for `sandbox: keep-id`), the id-remap copy is the likely cost. | Wait for/redo the warm (`podman run --rm --userns=keep-id:uid=1000,gid=1000 <image> true`), or restart fleet so the boot pre-warm pays it, or raise the knob. |
| `sandbox: preparing the keep-id id-remapped copy of image …` then a long pause at boot | The one-time per-image copy is being paid up front, on purpose. | Nothing — subsequent starts are ~2s. |
| `podman run: signal: killed` *without* the timeout wording | The kill came from outside the start timeout (OOM killer, manual kill, cgroup limit). | Check `journalctl` / `dmesg`, not this page. |

## Honest scope

- The pre-warm keys on the **local image ID**: an image re-pulled under the
  same tag while fleet is running is only warmed at the next boot; until then
  the tunable timeout and the named error are what you have.
- Podman's id-remap cache is keyed by (image, idmap) inside the rootless
  store; anything that clears that store (`podman system reset`) re-pays the
  copy on the next start, not at boot — same mitigation.
- The knob deliberately maps onto the kubernetes backend's pod-start ceiling
  too, so "sandbox start budget" is one concept across backends; it does NOT
  change kata/krun guest boot behavior.
