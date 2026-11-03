//go:build replay

package builtin

// An offline replay of the 27B experiment's tasks against a live model, to
// compare harnesses: scripts/replay-27b.sh runs it on this tree and on an
// older one (with that tree's replay_shim_test.go) and prints both. The
// model works in a scratch worktree of the commit the task was worked on;
// nothing is pushed. Scoring builds and tests the changed packages here.
//
//	WARDEN_REPLAY_URL=http://litellm…/v1 WARDEN_REPLAY_MODEL=qwen3.8-27b \
//	WARDEN_REPLAY_KEY_ENV=OPENAI_API_KEY WARDEN_REPLAY_OUT=/tmp/r.jsonl \
//	go test -tags replay -run TestReplay -timeout 3h ./internal/builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/meter"
	"github.com/nenkoru/warden/internal/skills"
)

// replayTask: one task of the experiment log, at the commit it was worked on.
type replayTask struct {
	Name, Base string
	Issue      github.Issue
	// Want: a file the change should touch (the scoring's "right place").
	Want []string
}

var replayTasks = []replayTask{
	{Name: "issue-18", Base: "9ca6f67", Want: []string{"internal/builtin/github_note.go", "internal/builtin/github_note_test.go"}, Issue: github.Issue{Number: 18, User: github.User{Login: "nenkoru"},
		Title: "chore: close duplicate digest issues #10 and #11",
		Body: "Issues #10 and #11 are both `[warden-digest]` auto-created digest issues that duplicate each other. They add noise to the open-issues list and are not actionable individually.\n\n" +
			"Close both with a comment noting they are duplicates of the digest mechanism, and ensure the digest act does not create a new issue when an open digest issue already exists.\n\n" +
			"**Files:** `packages/github-repo/skills/` (digest skill), the digest act logic in `internal/github/`\n" +
			"**Test:** Unit test that the digest act returns early (no issue created) when an open issue with the `[warden-digest]` label exists.\n" +
			"**Done when:** #10 and #11 are closed and the dedup test passes.\n\n**Why:** Open issues #10 and #11 are duplicate digest issues cluttering the tracker.\n\nKind: chore · size: S\n\nProposed by Warden's weekly roadmap pass (2026-W44)."}},
	{Name: "issue-16", Base: "223e533", Want: []string{"internal/builtin/github_acts.go"}, Issue: github.Issue{Number: 16, User: github.User{Login: "nenkoru"},
		Title: "fix(github): handle 405 merge-conflict in merge-pr act",
		Body: "The `merge-pr` act calls `PUT /repos/{owner}/{repo}/pulls/{n}/merge` and treats any non-2xx as a generic failure. When the PR has merge conflicts, GitHub returns 405. " +
			"The act should detect this specific status, label the PR `merge-conflict`, and skip the merge without marking the act as a hard failure.\n\n" +
			"**Files:** `internal/github/kernel.go` (merge-pr handler), `internal/github/kernel_test.go`\n" +
			"**Test:** Add a test case where the mock returns 405 and assert the act result is `skip` with the `merge-conflict` label applied, not an error.\n" +
			"**Done when:** The act no longer logs a failed act for 405 responses and the PR is labeled so a human or a later act can resolve it.\n\n" +
			"**Why:** Failed act 2026-10-27: merge-pr #5 returned 405 merge conflicts, logged as a failure.\n\nKind: fix · size: S\n\nProposed by Warden's weekly roadmap pass (2026-W44)."}},
}

// usage: the tokens of every response, as a meter that never refuses.
type usage struct {
	mu      sync.Mutex
	in, out int64
}

func (u *usage) Estimate(string, int, int) float64 { return 0 }
func (u *usage) Allow(float64) error               { return nil }
func (u *usage) Record(_ string, x meter.Usage) (float64, []meter.Alert) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.in += x.Input + x.CacheRead + x.CacheWrite
	u.out += x.Output
	return 0, nil
}

func TestReplay(t *testing.T) {
	url, model := os.Getenv("WARDEN_REPLAY_URL"), os.Getenv("WARDEN_REPLAY_MODEL")
	if url == "" || model == "" {
		t.Skip("WARDEN_REPLAY_URL and WARDEN_REPLAY_MODEL not set")
	}
	harness := os.Getenv("WARDEN_REPLAY_HARNESS")
	repo, _ := filepath.Abs("../..")
	if r := os.Getenv("WARDEN_REPLAY_REPO"); r != "" {
		repo = r
	}
	sk, err := skills.Load(filepath.Join(repo, "packs/github-repo/skills"))
	if err != nil {
		t.Fatal(err)
	}
	runs := 1
	fmt.Sscan(os.Getenv("WARDEN_REPLAY_RUNS"), &runs)
	only := os.Getenv("WARDEN_REPLAY_ONLY")
	for _, task := range replayTasks {
		if only != "" && !strings.Contains(","+only+",", ","+task.Name+",") {
			continue
		}
		for run := range runs {
			t.Run(fmt.Sprintf("%s/%d", task.Name, run), func(t *testing.T) {
				wt := filepath.Join(t.TempDir(), "wt")
				if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "--detach", wt, task.Base).CombinedOutput(); err != nil {
					t.Fatalf("worktree: %v %s", err, out)
				}
				defer exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run()
				env := &Env{Params: map[string]string{"repo": "nenkoru/warden", "checks_mode": "ci"}, Skills: skills.Index(sk),
					Tools: []brain.Tool{{Name: "read_skill", Description: "Load one of the domain's skills by name (its full instructions).",
						Schema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}},
						Run: func(_ context.Context, a map[string]any) (string, bool) {
							for _, s := range sk {
								if s.Name == a["name"] {
									return s.Body, true
								}
							}
							return fmt.Sprintf("no skill %v", a["name"]), false
						}}}}
				u := &usage{}
				var log strings.Builder
				cfg := brain.EndpointConfig{Provider: "openai", BaseURL: url, Model: model, APIKeyEnv: os.Getenv("WARDEN_REPLAY_KEY_ENV"), Meter: u, Log: &log}
				replayConfig(&cfg)
				st := &brain.Stats{}
				start := time.Now()
				note, berr := runCoder(context.Background(), env, NamedBrain{Name: "local", Brain: brain.Endpoint(cfg)}, wt, replayIssueTask(task.Issue, wt), st)
				wall := time.Since(start)
				changed, _ := changedFiles(context.Background(), wt)
				gofmtChanged(wt, changed)
				rec := map[string]any{"harness": harness, "task": task.Name, "run": run, "wall_s": int(wall.Seconds()), "tokens_in": u.in, "tokens_out": u.out,
					"changed": changed, "right_place": touches(changed, task.Want), "err": fmt.Sprint(berr), "note": firstLines(note, 8), "build": score(wt, changed)}
				for k, v := range st.Data() {
					rec[k] = v
				}
				_ = os.WriteFile(filepath.Join(os.TempDir(), "warden-replay-"+harness+"-"+task.Name+fmt.Sprint(run)+".log"), []byte(log.String()), 0o644)
				b, _ := json.Marshal(rec)
				t.Logf("%s", b)
				if out := os.Getenv("WARDEN_REPLAY_OUT"); out != "" {
					f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
					if err == nil {
						fmt.Fprintf(f, "%s\n", b)
						f.Close()
					}
				}
			})
		}
	}
}

func touches(changed, want []string) bool {
	for _, c := range changed {
		for _, w := range want {
			if c == w {
				return true
			}
		}
	}
	return false
}

// score: "none" (no Go change), "green" (the changed packages build, vet
// and test), else the failing step.
func score(wt string, changed []string) string {
	pkgs := map[string]bool{}
	for _, c := range changed {
		if strings.HasSuffix(c, ".go") {
			pkgs["./"+filepath.Dir(c)] = true
		}
	}
	if len(pkgs) == 0 {
		return "none"
	}
	var list []string
	for p := range pkgs {
		list = append(list, p)
	}
	for _, step := range [][]string{{"build", "./..."}, {"vet"}, {"test", "-count=1"}} {
		args := append([]string{}, step...)
		if step[0] != "build" {
			args = append(args, list...)
		}
		cmd := exec.Command("go", args...)
		cmd.Dir = wt
		if out, err := cmd.CombinedOutput(); err != nil {
			return step[0] + " red: " + tail(string(out), 300)
		}
	}
	return "green"
}
