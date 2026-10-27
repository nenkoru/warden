---
name: server-triage
description: Read-only triage of a Linux server's failed units and journal errors; propose fixes, never change services on your own.
---
# Server triage (read-only)

1. A failed unit: `systemctl status UNIT`, `journalctl -u UNIT -n 100` — find the first error, not the last.
2. Disk: `du -xh --max-depth=1 /var | sort -h`, old journals (`journalctl --disk-usage`), docker images — propose, don't delete.
3. Never restart, stop or edit services, Caddy, or other agents' daemons yourself: write the proposed command and why.
