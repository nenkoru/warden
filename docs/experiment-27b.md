# The 27B experiment log

Warden maintains this repository with a local model doing all of the work, features included. Every task — an issue Warden works, from the first attempt through fix rounds to the merge, a give-up or a revert — is recorded in Warden's state and rendered here once a day by its `publish-log` act. The owner and Claude read it every two or three days to tune the prompts, tools, context and skills. How to read it: the weekly summary first (first-push green is the share of PRs whose CI passed without a fix round; tool-call errors are calls to a tool that does not exist or with arguments that do not parse, per tool call; wall time and tokens are the model's work on the task), then each day's tasks and its failures with their categories (tool-call, context-limit, wrong-edit, loop, timeout, gave-up, other) and one-line reasons. Idle hours are the model's off-hours, an unreachable upstream or a spent budget: time it could not work, never a failure. A revert marks its task's row on the day and in the week the task began (and counts as reverted in that week), with a note on the day it happened. "so far" marks the period still running.

Days and weeks are in Europe/Moscow.

## Weekly summary

| week | tasks | merged | first-push green | avg attempts | avg fix rounds | median wall time | tokens in / out | tool-call errors / call | reverted | top failures | idle hours |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 2026-W44 (so far) | 1 | 0 | — | 1.0 | 0.0 | 7s | 0 / 0 | — | 0 | — | 0.2 |

## Days

### 2026-10-28 (so far)

| task | PR | size | brain / model | attempts | steps | tool calls | tool-call errors | wall time | tokens in / out | CI first push | fix rounds | merged | reverted | review comments | failures |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| PR #12 | #12 | — | delegate | 1 | 0 | 0 | 0 | 7s | 0 / 0 | — | 0 | no | no | 0 | — |

Idle: 0.2 h (off-hours 0.2 h).
