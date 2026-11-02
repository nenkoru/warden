//go:build replay

package builtin

import (
	"encoding/json"
	"os"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/github"
)

// This harness's side of replay_test.go (scripts/replay/old_shim.go.txt is
// the side for a tree before it).

func replayIssueTask(is github.Issue, wt string) string { return issueTask(is, wt) }

// replayConfig: WARDEN_REPLAY_EXTRA (a JSON object) as the request's extra fields.
func replayConfig(c *brain.EndpointConfig) {
	if s := os.Getenv("WARDEN_REPLAY_EXTRA"); s != "" {
		_ = json.Unmarshal([]byte(s), &c.ExtraBody)
	}
}
