# The 27B experiment log

Warden maintains this repository with a local model doing all of the work, features included. Every task — an issue Warden works, from the first attempt through fix rounds to the merge, a give-up or a revert — is recorded in Warden's state and rendered here once a day by its `publish-log` act. The owner and Claude read it every two or three days to tune the prompts, tools, context and skills. How to read it: the weekly summary first (first-push green is the share of PRs whose CI passed without a fix round; tool-call errors are calls to a tool that does not exist or with arguments that do not parse, per tool call; wall time and tokens are the model's work on the task), then each day's tasks and its failures with their categories (tool-call, context-limit, wrong-edit, loop, timeout, gave-up, other) and one-line reasons. Idle hours are the model's off-hours, an unreachable upstream or a spent budget: time it could not work, never a failure. A revert marks its task's row on the day and in the week the task began (and counts as reverted in that week), with a note on the day it happened. "so far" marks the period still running.

Days and weeks are in Europe/Moscow.

## Weekly summary

| week | tasks | merged | first-push green | avg attempts | avg fix rounds | median wall time | tokens in / out | tool-call errors / call | reverted | top failures | idle hours |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 2026-W44 (so far) | 7 | 3 | 66% (2/3) | 2.1 | 0.1 | 7s | 13.7M / 21.2k | 0.04 (15/338) | 0 | loop 6, tool-call 4, wrong-edit 2 | 20.9 |

## Days

### 2026-10-31 (so far)

No task worked.

Idle: 0.2 h (off-hours 0.2 h).

### 2026-10-30

| task | PR | size | brain / model | attempts | steps | tool calls | tool-call errors | wall time | tokens in / out | CI first push | fix rounds | merged | reverted | review comments | failures |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| #18 | #24 | M | — | 0 | 0 | 0 | 0 | 8s | 0 / 0 | red | 0 | yes | no | 0 | — |

Idle: 7.5 h (off-hours 7.5 h).

### 2026-10-29

| task | PR | size | brain / model | attempts | steps | tool calls | tool-call errors | wall time | tokens in / out | CI first push | fix rounds | merged | reverted | review comments | failures |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| PR #22 | #22 | — | delegate | 1 | 0 | 0 | 0 | 6s | 0 / 0 | — | 0 | no | no | 0 | — |
| PR #23 | #23 | — | delegate | 1 | 0 | 0 | 0 | 4s | 0 / 0 | — | 0 | no | no | 0 | — |
| #18 | #24 | S | local / qwen3.8-27b-local | 3 | 69 | 87 | 6 | 12m 42s | 4.4M / 9.4k | red | 1 | no | no | 0 | wrong-edit, loop |

Failures:

- 09:43 #18 new: **loop** — issue #18: the brain local failed (stopped after 25 steps (max_steps)); tried again next tick
- 10:05 #18 fix: **wrong-edit** — PR #24: fix round pushed (brain: local)

Idle: 7.5 h (off-hours 7.5 h).

### 2026-10-28

| task | PR | size | brain / model | attempts | steps | tool calls | tool-call errors | wall time | tokens in / out | CI first push | fix rounds | merged | reverted | review comments | failures |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| PR #12 | #12 | — | delegate | 1 | 0 | 0 | 0 | 7s | 0 / 0 | — | 0 | no | no | 0 | — |
| PR #14 | #14 | — | delegate | 1 | 0 | 0 | 0 | 4s | 0 / 0 | — | 0 | no | no | 0 | — |
| #16 | #19 | S | local / qwen3.8-27b-local | 1 | 18 | 22 | 1 | 4m 12s | 912.9k / 3.0k | green | 0 | yes | no | 0 | — |
| #17 | #20 | S | local / qwen3.8-27b-local | 1 | 5 | 7 | 0 | 45s | 26.2k / 772 | green | 0 | yes | no | 0 | — |
| #18 | — | S | local / qwen3.8-27b-local | 6 | 148 | 222 | 8 | 23m 50s | 8.3M / 8.0k | — | 0 | no | no | 0 | tool-call, wrong-edit, loop |

Failures:

- 08:55 #18 new: **tool-call, loop** — issue #18: the brain local failed (stopped after 25 steps (max_steps)); tried again next tick
- 09:03 #18 new: **tool-call, loop** — issue #18: the brain local failed (stopped after 25 steps (max_steps)); tried again next tick
- 09:14 #18 new: **loop** — issue #18: the brain local failed (stopped after 25 steps (max_steps)); tried again next tick
- 09:24 #18 new: **tool-call, loop** — issue #18: the brain local failed (stopped after 25 steps (max_steps)); tried again next tick
- 09:35 #18 new: **loop** — issue #18: the brain local failed (stopped after 25 steps (max_steps)); tried again next tick
- 09:44 #18 new: **tool-call, wrong-edit** — issue #18: the brain (local) made no change

Idle: 5.7 h (off-hours 5.7 h).
