# The 27B experiment log

Warden maintains this repository with a local model doing all of the work, features included. Every task — an issue Warden works, from the first attempt through fix rounds to the merge, a give-up or a revert — is recorded in Warden's state and rendered here once a day by its `publish-log` act. The owner and Claude read it every two or three days to tune the prompts, tools, context and skills. How to read it: the weekly summary first (first-push green is the share of PRs whose CI passed without a fix round; tool-call errors are calls to a tool that does not exist or with arguments that do not parse, per tool call; wall time and tokens are the model's work on the task), then each day's tasks and its failures with their categories (tool-call, context-limit, wrong-edit, loop, timeout, gave-up, other) and one-line reasons. Idle hours are the model's off-hours, an unreachable upstream or a spent budget: time it could not work, never a failure. A revert marks its task's row on the day and in the week the task began (and counts as reverted in that week), with a note on the day it happened. "so far" marks the period still running.

Days and weeks are in Europe/Moscow.

## Weekly summary

| week | tasks | merged | first-push green | avg attempts | avg fix rounds | median wall time | tokens in / out | tool-call errors / call | reverted | top failures | idle hours |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 2026-W44 (so far) | 2 | 1 | 0% (0/1) | 2.0 | 0.5 | 14m 25s | 822.0k / 55.0k | 0.09 (7/80) | 0 | wrong-edit 2, context-limit 1, loop 1 | 6.0 |
| 2026-W43 | 1 | 1 | 100% (1/1) | 1.0 | 0.0 | 4m 10s | 120.0k / 8.0k | 0.00 (0/11) | 1 | — | 7.0 |

## Days

### 2026-10-28 (so far)

No task worked.

Reverted at 12:00: #3 (PR #4) — its row is under 2026-10-23.

### 2026-10-27

| task | PR | size | brain / model | attempts | steps | tool calls | tool-call errors | wall time | tokens in / out | CI first push | fix rounds | merged | reverted | review comments | failures |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| #7 | — | — | local / qwen3.8-27b-local | 2 | 40 | 44 | 4 | 15m 30s | 432.0k / 30.0k | — | 0 | no | no | 0 | context-limit, wrong-edit, loop |

Failures:

- 09:00 #7 new: **context-limit** — issue #7: the brain local failed ([url] 400: maximum context length is 32768 tokens); tried again next tick
- 09:30 #7 new: **wrong-edit, loop** — issue #7: the brain (local) made no change \| @​someone

Idle: 6.0 h (off-hours 5.0 h, unavailable 1.0 h).

### 2026-10-26

| task | PR | size | brain / model | attempts | steps | tool calls | tool-call errors | wall time | tokens in / out | CI first push | fix rounds | merged | reverted | review comments | failures |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| #5 | #6 | L | local / qwen3.8-27b-local | 2 | 38 | 36 | 3 | 13m 20s | 390.0k / 25.0k | red | 1 | yes | no | 0 | wrong-edit |

Failures:

- 09:40 #5 fix: **wrong-edit** — PR #6: fix round pushed (brain: local)

### 2026-10-23

| task | PR | size | brain / model | attempts | steps | tool calls | tool-call errors | wall time | tokens in / out | CI first push | fix rounds | merged | reverted | review comments | failures |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| #3 | #4 | S | local / qwen3.8-27b-local | 1 | 12 | 11 | 0 | 4m 10s | 120.0k / 8.0k | green | 0 | yes | yes | 1 | — |

Idle: 7.0 h (off-hours 7.0 h).
