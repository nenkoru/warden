package github

import "testing"

func TestGuard(t *testing.T) {
	c := &Client{Repo: "acme/proj"}
	p := "/repos/acme/proj"
	allowed := []struct {
		m, path string
		body    any
	}{
		{"GET", p, nil}, {"GET", p + "/pulls?state=open", nil}, {"GET", "/user", nil}, {"GET", p + "/rules/branches/main", nil},
		{"GET", p + "/compare/main...claude/pickup-title", nil},
		{"POST", p + "/issues/3/comments", nil}, {"POST", p + "/pulls", nil}, {"PATCH", p + "/pulls/4", nil},
		{"PUT", p + "/pulls/4/merge", nil}, {"POST", p + "/pulls/4/reviews", nil}, {"POST", p + "/issues", nil},
		{"PATCH", p + "/issues/comments/77", nil}, {"GET", p + "/releases?per_page=30", nil}, {"GET", p + "/releases/assets/5", nil},
		{"GET", p + "/attestations/sha256:abc", nil},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/tags/v0.2.0", "sha": "abc"}},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/tags/v0.2.0-rc.1", "sha": "abc"}},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/tags/v0.2.0-rc.12", "sha": "abc"}},
		{"POST", "/graphql", map[string]any{"query": "mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { number } } }"}},
	}
	for _, a := range allowed {
		if why := c.Guard(a.m, a.path, a.body); why != "" {
			t.Errorf("%s %s refused: %s", a.m, a.path, why)
		}
	}
	refused := []struct {
		m, path string
		body    any
	}{
		{"PATCH", p, nil}, {"DELETE", p, nil}, {"GET", p + "/branches/main/protection", nil}, {"PUT", p + "/branches/main/protection", nil},
		{"POST", p + "/rulesets", nil}, {"GET", p + "/rulesets", nil}, {"PUT", p + "/collaborators/eve", nil}, {"POST", p + "/keys", nil},
		{"POST", p + "/hooks", nil}, {"GET", p + "/actions/secrets", nil}, {"PUT", p + "/contents/README.md", nil}, {"POST", p + "/merges", nil},
		{"DELETE", p + "/git/refs/heads/main", nil}, {"PATCH", p + "/git/refs/heads/main", nil},
		{"DELETE", p + "/issues/comments/77", nil}, {"PATCH", p + "/issues/77", nil}, {"PATCH", p + "/issues/comments/x", nil},
		{"POST", p + "/releases", nil}, {"PATCH", p + "/releases/5", nil},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/heads/main", "sha": "abc"}},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/tags/v0.2.0-rc", "sha": "abc"}},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/tags/v0.2.0-rc.1x", "sha": "abc"}},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/tags/v0.2.0-beta.1", "sha": "abc"}},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/tags/v0.2.0-rc.1\n", "sha": "abc"}},
		{"POST", p + "/git/refs", map[string]string{"ref": "refs/tags/latest", "sha": "abc"}},
		{"POST", "/repos/other/repo/issues", nil}, {"POST", "/user/repos", nil},
		{"POST", "/graphql", map[string]any{"query": "mutation { updateRepository(input: {repositoryId: \"x\"}) { repository { id } } }"}},
	}
	for _, r := range refused {
		if c.Guard(r.m, r.path, r.body) == "" {
			t.Errorf("%s %s allowed", r.m, r.path)
		}
	}
	anon := &Client{Repo: "acme/proj", anon: true}
	if anon.Guard("GET", p+"/releases", nil) != "" || anon.Guard("POST", p+"/issues/3/comments", nil) == "" || anon.Guard("PATCH", p+"/issues/comments/7", nil) == "" {
		t.Error("an anonymous client must read and never write")
	}
	for s, want := range map[string]bool{"HEAD:refs/heads/warden/issue-3": true, "+HEAD:refs/heads/warden/x": true, "HEAD:refs/heads/main": false,
		"HEAD:main": false, "HEAD:refs/tags/v1.0.0": false, "HEAD:refs/heads/warden/../main": false} {
		if AllowedPush(s) != want {
			t.Errorf("AllowedPush(%q) = %v", s, !want)
		}
	}
}
