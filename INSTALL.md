# Installing Warden as an always-on service

One user-level service, nothing system-wide. Layout (the same on macOS and Linux):

```
~/.warden/bin/warden              the binary
~/.warden/packs/<pack>/           the domain pack(s) it uses (copied from warden/packs)
~/.warden/<name>/warden.toml      one instance; its state in ~/.warden/<name>/.warden/
```

`domain = "pack:NAME"` is found by walking up from the instance folder: `~/.warden/packs/NAME`.

## Get the binary

Download a release archive (`warden_<version>_<os>_<arch>.tar.gz`: the binary, the packs, LICENSE) from the GitHub
releases page and check it against `checksums.txt` (each release also has SBOMs and a build provenance attestation:
`gh attestation verify warden_….tar.gz --repo nenkoru/warden`). Or build it:

```bash
git clone https://github.com/nenkoru/warden && cd warden
go build -o warden ./cmd/warden                                                       # this machine
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o warden-linux-amd64 ./cmd/warden    # a Linux server
```

## macOS (launchd user agent)

```bash
mkdir -p ~/.warden/bin ~/.warden/packs ~/.warden/mac
cp warden ~/.warden/bin/ && cp -R packs/macos ~/.warden/packs/
~/.warden/bin/warden init --pack macos --name mac --config ~/.warden/mac/warden.toml   # mode = dry-run
~/.warden/bin/warden tick  --config ~/.warden/mac/warden.toml                          # look at the first report
~/.warden/bin/warden units --config ~/.warden/mac/warden.toml                          # writes the plist (does not load it)
cp ~/.warden/mac/.warden/units/dev.warden.mac.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/dev.warden.mac.plist
```

To stop and remove the service:

```bash
launchctl unload ~/Library/LaunchAgents/dev.warden.mac.plist && rm ~/Library/LaunchAgents/dev.warden.mac.plist
```

The state stays in `~/.warden/mac/.warden`.

## Linux (systemd --user)

```bash
mkdir -p ~/.warden/bin ~/.warden/packs ~/.warden/server ~/.config/systemd/user
install -m 0755 warden-linux-amd64 ~/.warden/bin/warden && cp -R packs/linux-server ~/.warden/packs/
~/.warden/bin/warden init --pack linux-server --name server --config ~/.warden/server/warden.toml
~/.warden/bin/warden tick  --config ~/.warden/server/warden.toml
~/.warden/bin/warden units --config ~/.warden/server/warden.toml
cp ~/.warden/server/.warden/units/dev.warden.server.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now dev.warden.server.timer
loginctl enable-linger "$USER"     # optional: keep user timers running while logged out (may need the admin)
```

Notes:

- The `linux-server` pack is read-only and has no acts besides `propose`.
- `systemctl list-units` and `journalctl -p err` need no root for the user's own view. The system journal may need
  the user in group `systemd-journal` or `adm`; otherwise those findings are empty, and the report says so.
- **Sandbox fails closed.** Without `bwrap`, the read-only pack still observes and reports, but anything that would
  execute an act is refused until `sandbox = "none"` is set explicitly.

## Updates (optional)

An `[update]` section lets an installed Warden update itself from verified releases (checksum and Sigstore build
provenance; the previous binary kept and restored when the new one does not finish a tick healthy). Keep the binary a
plain file the service's user can write (`~/.warden/bin/warden` above). See
[docs/usecases/self-update.md](docs/usecases/self-update.md).

## Talking to other agents (optional, nothing is wired by default)

- **An agent harness reads Warden:** add `~/.warden/bin/warden mcp --config ~/.warden/mac/warden.toml` as an MCP
  server in that harness. It gets the tools `warden_status`, `warden_report`, `warden_pending`, `warden_approve` and
  `warden_signal`.
- **Agents write to Warden:** `warden serve --config …` serves `POST /signal` and A2A on `[serve] addr`. Each agent
  gets a bearer token; tiers come from `[serve]` owners and trusted lists.
- **Warden writes to agents:** a `Report` / `Ask` hook runs any command, for example a chat or chain CLI. Replies on
  the owner's behalf stay dry-run until `replies = true`.
- **A chain network or a mail system:** attach its MCP server or CLI the same way. Warden has no built-in network.
