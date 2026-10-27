# A Linux server

**Packs:** `packs/linux-server` (general) and `packs/linux-host` (a GPU / model-serving host). Both are read-only:
nothing on the host is changed, and every fix is a proposal file for you. There is no recorded demo for these packs in
`examples/` yet; the report lines below show the shape of their findings.

## linux-server

Observes free disk space, failed systemd units, recent journal errors (grouped by line, so a flood is one finding with
a count), and the installed dpkg packages as an inventory for CISA advisories.

```toml
# ~/.warden/server/warden.toml — written by `warden init --pack linux-server --name server`
name    = "server"
domain  = "pack:linux-server"
params  = { min_free_gb = "10", since = "-6h" }
mode    = "act"                                 # the only act is "propose": it writes a file
wake    = { every = "1h", listen = false }
rules   = [{ act = "propose", then = "auto" }]
```

- `systemctl list-units` and `journalctl -p err` need no root for the user's own view. The system journal may need the
  user in group `systemd-journal` or `adm`; otherwise those findings are empty.
- Install it as a `systemd --user` timer: see [INSTALL.md](../../INSTALL.md).

What a tick with a failed unit and low disk produces (the shape, not a recorded run):

```markdown
**Worth your attention: 2** (done: 1, waiting for you: 0, important: 1).

## Done

- ✅ **failed unit: backup.service** — proposal written: ~/.warden/server/.warden/proposals/….md

## Important

- **Low disk space: 7.9 GB free** — medium · `…`
```

## linux-host

For a host that exists to serve something (here an OpenAI-compatible model server): disk below `min_free_pct`, failed
units of the system and of Warden's own user, GPU temperature above `gpu_max_c` and uncorrected ECC errors
(`nvidia-smi`), and the health endpoint of the service. During "quiet hours" a down service is a digest line instead of
an incident (it may be stopped on purpose for maintenance or experiments).

```toml
name    = "gpu-box"
domain  = "pack:linux-host"
params  = { service_url = "http://127.0.0.1:8000/health", quiet = "0030-0800", tz = "UTC", quiet_weekends = "no", gpu_max_c = "85" }
mode    = "act"
wake    = { every = "15m", listen = false }
rules   = [{ act = "propose", then = "auto" }]

# a brain that also lives on the box, but never at night when the GPU is busy:
# brain = { provider = "openai", base_url = "http://127.0.0.1:8000/v1", model = "my-model", quiet = "0030-0800", tz = "UTC" }
```

## The sandbox on Ubuntu 24.04

Installing `bubblewrap` is not enough on Ubuntu 24.04: AppArmor
(`kernel.apparmor_restrict_unprivileged_userns = 1`) denies it a user namespace and every run fails with
`setting up uid map: Permission denied`. Warden detects this and reports "no sandbox"; read-only packs still observe
and report, but acts are refused. The narrow fix (your decision, needs sudo) is a profile for bwrap alone:

```
# /etc/apparmor.d/bwrap
abi <abi/4.0>,
include <tunables/global>
profile bwrap /usr/bin/bwrap flags=(unconfined) {
  userns,
}
```

then `sudo apparmor_parser -r /etc/apparmor.d/bwrap`.
