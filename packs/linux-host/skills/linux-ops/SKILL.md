---
name: linux-ops
description: How to analyse a Linux host incident (failed unit, full disk, GPU, model server down) without changing anything.
---
# Linux host incidents

1. Name the likely cause from what the finding says; say what to run to confirm it (`systemctl status UNIT`,
   `journalctl -u UNIT -n 100`, `df -h`, `du -xh --max-depth=1 /var | sort -h`, `nvidia-smi`), never run it yourself.
2. Propose the smallest fix as commands for the human, with the risk of each; prefer restarts over reinstalls and
   cleanups of regenerable data (caches, old logs) over anything else. Never propose deleting user data.
3. For a model server that does not answer: check the unit, the port, GPU memory (`nvidia-smi`), and the last log lines.
