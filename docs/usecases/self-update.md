# Warden updating itself

Every Warden in a fleet can keep itself up to date from the GitHub releases of `nenkoru/warden` — on the owner's Mac
(launchd), on Linux hosts (`systemd --user`) and in Kubernetes pods. It is off until `[update]` says otherwise, it
never installs a release it cannot verify, and it puts the previous binary back when the new one does not reach the
end of a healthy tick.

## How it works

Inside the tick, never in a separate process:

1. **At tick start** — before observing, so whatever happens is in this tick's report:
   - an install still in flight is checked (see *Rollback*);
   - at most every `update.every` (default 6h) the releases are listed: drafts are skipped, prereleases only on the
     canary channel, and the newest version above the running one is taken — on the stable channel only once it has
     been published for `canary_hours` (default 24) **and** no open issue labeled `warden-rollback` names it. A
     version that was rolled back on this instance is never taken again. A `dev` build never updates unless
     `allow_dev = true`.
   - the release's archive for this OS and architecture (`warden_<version>_<os>_<arch>.tar.gz`) and `checksums.txt`
     are downloaded (size caps, timeouts, Warden's User-Agent) and **verified** (below).
2. **At the end of a healthy tick** (the tick saved its state and returned no error), a verified release is installed:
   the `warden` binary alone is taken out of the archive (no path traversal, no links, a size cap) and written as
   `<bin>.new` (0755); the new binary must pass a preflight — `warden update --preflight --config warden.toml` prints
   its version and loads this config — then the running binary is kept as `<bin>.prev` and `<bin>.new` is renamed
   over `<bin>` (same folder: atomic). `<state>/update/state.json` records `{from, to, at, pending: true, attempts}`.
3. The running process does not re-exec. The next tick is the new binary's: a timer unit (launchd `StartInterval`, a
   systemd timer) simply starts it; a long-lived `warden listen` / `warden serve` exits cleanly after the tick and its
   supervisor (launchd `KeepAlive`, systemd `Restart=always` — what `warden units --listen` writes) starts the new one.

The report's *Under the hood* says where things stand:
`version v0.2.0 (update: v0.3.0 available | up to date | installed v0.3.0, pending health (tick 1) | rolled back from v0.3.0)`.

`mode = "propose"` stops after the check: the verified version is a finding in the report (once per version) and
nothing is installed. `warden update --check --config warden.toml` checks and verifies now; `warden update` also
installs (not on Kubernetes, not while another install waits for its first healthy tick).

## The verification chain

An unverifiable release is **never** installed; every link fails closed, and a release that fails is a finding that
pings ("warden release vX fails verification — not installed").

1. **Checksum.** The archive's sha256 must be its line in the release's `checksums.txt`.
2. **Build provenance.** The release workflow (`.github/workflows/release.yml`) attests every file in `checksums.txt`
   with `actions/attest-build-provenance`. Warden fetches the attestations for `sha256:<archive digest>`
   (`GET /repos/{repo}/attestations/sha256:…`) and verifies each Sigstore bundle with `sigstore-go` against the
   public-good trusted root:
   - the certificate chains to Fulcio, has a signed certificate timestamp, and the signature is in the Rekor
     transparency log (an inclusion proof or promise, with its integrated time as the signing time);
   - the signer is `https://github.com/<repo>/.github/workflows/release.yml@refs/tags/<tag>` — that workflow, run for
     exactly that tag (`signer_ref` may loosen the ref to a regexp) — through the OIDC issuer
     `https://token.actions.githubusercontent.com`;
   - the statement is SLSA provenance v1 and its subjects include the archive's digest.

   The trusted root comes through TUF (the root of trust embedded in sigstore-go), cached in `<state>/update/tuf` and
   refreshed daily. Offline, the cached TUF metadata is used while it is still validly signed and unexpired (about a
   week); there is no unverified fallback.
3. **cosign, when present.** If a `cosign` binary is on `PATH` and the release carries a `.bundle` (or `.sig` + `.pem`)
   for the archive or `checksums.txt`, `cosign verify-blob` must pass with the same identity. Their absence is fine.

## Rollback

`state.json` counts the new binary's attempts:

- at tick start, while an install is pending and the running binary is the new one, `attempts` goes up by one;
- at a healthy tick end, the install is confirmed: `pending = false`, `healthy_since`;
- at tick start, if it is still pending and `attempts ≥ 1`, the new binary began a tick before and never ended one
  healthy (it crashed, hung and was killed, or failed to save): `<bin>.prev` is put back, the version is recorded as bad
  (never installed again here) and a **`rollback` finding of severity high** is reported — it always pings (a triage
  floor). A long-lived process exits after that tick so the restored binary runs.

A binary that cannot even start is caught earlier, by the preflight. If an install was made by hand (`warden update`)
while a `listen` process runs the old binary, that process's ticks count nothing and it asks for one restart.

## Kubernetes

Detected by `KUBERNETES_SERVICE_HOST` (or `kubernetes = true`). The pod's image is pinned by digest and its root
filesystem is read-only, so **the binary is never touched**. The release is verified as above and proposed as a
finding: "update available: vX — set image to `<image>@sha256:…` after building it". If a release publishes
`warden_<version>_image-digest.txt` (one line, `sha256:…`; listed in `checksums.txt`, so attested like the archive),
the finding names that digest.

With `mode = "install"`, `grant_image_bump = true`, `image`, `deployment` (and `container`, default `warden`) set, a
`kubectl` in `PATH` and a published, verified image digest, the instance runs
`kubectl -n <its namespace> set image deployment/<deployment> <container>=<image>@<digest>` at the end of a healthy
tick. Anything missing → "image bump not granted". This grant path is deliberately minimal (a marked TODO in
`internal/update/k8s.go`): no ask before the first bump and no rollout watch or `rollout undo` yet. The release workflow
does not publish an image digest today, so in practice Kubernetes instances propose.

## The fleet beacon (optional)

With `beacon_token_file` (a token that may write issues), after a healthy tick each instance keeps **one** comment on
the open issue labeled `warden-fleet` titled "Warden fleet" (created and pinned when missing): a line for people and a
marker for machines,
`<!-- warden-fleet {"name":"buvg01","version":"v0.3.0","channel":"canary","healthy_since":"…","ticks_ok":42} -->`.
It is rewritten in place (`PATCH /repos/{repo}/issues/comments/{id}` — the guard allows the path, the client first
checks that the comment is the token user's own) at most hourly, or at once when the version or a rollback changes.
On a rollback the instance also opens "release vX rolled back on <name>" labeled `warden-rollback` — which holds every
stable instance back from vX while it stays open. `update.FleetStatus` reads the beacons back (only comments by the
token's own user or by owners, members and collaborators), and the maintainer's release waits for the canary's
health (below).

## Canary before tagging

The Warden that maintains the repository (the `github-repo` pack with `params.canary = "buvg01"`) does not tag a
release straight away. After the release PR merges it tags `vX.Y.Z-rc.N` on the merge commit; the release workflow
publishes it as a prerelease, which only `channel = "canary"` instances take. The canary installs it, verifies it like
any release and reports on its beacon. When that beacon — written by the maintainer's token user or the owner, nobody
else counts — says the canary runs the rc, healthy for the maintainer's `canary_hours` (default 24) and at least
`canary_min_ticks` (default 6) ticks, without a rollback, its beacon written within `canary_stale_hours` (default 3),
and `main` is green, the maintainer tags `vX.Y.Z` on the same
commit; that final release is what the stable channel takes (after its own `canary_hours` from publishing, which may
now be short). A rollback of the rc on the canary — or an open `warden-rollback` issue naming it — means no final tag,
a ping, an issue for the owner, and a new attempt only after new PRs are merged. See
[the maintainer](github-maintainer.md#canary-before-tagging).

## Configuration

```toml
[update]
mode         = "install"            # off (default) | install | propose
channel      = "stable"             # stable | canary (prereleases too, no waiting)
canary_hours = 24                   # stable: wait this long after publishing
every        = "6h"                 # between checks (≥ 1h)
# repo = "nenkoru/warden"  api = "https://api.github.com"
# token_file = "~/.config/warden/github-read"   # optional: reads with a token instead of anonymously
# bin = "~/.warden/bin/warden"                   # default: this executable (a regular file, not a symlink)
# beacon_token_file = "~/.config/warden/beacon-token"
# allow_dev = false  workflow = ".github/workflows/release.yml"  signer_ref = "refs/tags/{tag}"
# Kubernetes: kubernetes = true  image = "…"  deployment = "warden"  container = "warden"  grant_image_bump = false
```

**The canary — `buvg01`** (Linux, `systemd --user`, `~/warden/bin/warden`, `warden units --listen`): takes every
release (prereleases too) as soon as it is published, and tells the fleet how it went.

```toml
[update]
mode              = "install"
channel           = "canary"
bin               = "~/warden/bin/warden"
beacon_token_file = "~/.config/warden/beacon-token"
```

**The owner's Mac** (launchd, `~/.warden/bin/warden`): stable, a day behind the canary, never what the canary rolled
back.

```toml
[update]
mode         = "install"
channel      = "stable"
canary_hours = 24
bin          = "~/.warden/bin/warden"
```

**A pod** (`git.bayfut.net/nenkoru/warden@sha256:…`): proposes.

```toml
[update]
mode  = "propose"
image = "git.bayfut.net/nenkoru/warden"
```

## The owner's one-time steps

1. Releases are published by the release workflow (`release.draft: false`; self-update never sees drafts); an
   `-rc.N` tag is a prerelease. Publishing starts the stable channel's clock (`canary_hours` counts from
   `published_at`). Without the maintainer's canary gate, a release reaches the canary and, a day later, everyone.
2. Create the labels `warden-rollback` and `warden-fleet` in the repository.
3. For the beacon: a fine-grained token for `nenkoru/warden` only, *Issues: read and write*, in a file `chmod 600`.
4. Install the binary as a plain file the service's user can write (`~/.warden/bin/warden`, `~/warden/bin/warden`),
   not through a package manager's symlink, and load the units (`warden units [--listen]`).
5. To stop a release fleet-wide: open an issue labeled `warden-rollback` naming the version. Stable instances skip it
   while the issue is open.
6. Kubernetes with the grant: a Role allowing `patch` on `deployments` (`resourceNames: [<deployment>]`) in the
   namespace for the pod's service account, `kubectl` in the image, and an image-digest asset in the release.
