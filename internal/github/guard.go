package github

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The guard: what Warden's GitHub client may do at all, whatever the token
// allows and whatever the repository enforces. Writes are an allowlist (the
// few calls the maintainer needs); administration is denied even for reads.
// A merge into the default branch happens only through PUT …/pulls/N/merge,
// a ref is created only for a v* tag, and git pushes are checked separately
// (only warden/* branches; see AllowedPush).

var (
	writeAllow = []struct {
		method string
		re     *regexp.Regexp
	}{
		{"POST", regexp.MustCompile(`^/issues/\d+/comments$`)},
		{"POST", regexp.MustCompile(`^/issues/\d+/labels$`)},
		{"POST", regexp.MustCompile(`^/issues$`)},
		{"POST", regexp.MustCompile(`^/pulls$`)},
		{"PATCH", regexp.MustCompile(`^/pulls/\d+$`)},
		{"PATCH", regexp.MustCompile(`^/issues/comments/\d+$`)}, // own comments only: EditComment checks the author
		{"POST", regexp.MustCompile(`^/pulls/\d+/reviews$`)},
		{"PUT", regexp.MustCompile(`^/pulls/\d+/merge$`)},
		{"POST", regexp.MustCompile(`^/git/refs$`)}, // tags only: checked on the body
	}
	adminDeny  = regexp.MustCompile(`^/(branches/[^/]+/protection|rulesets|rules/(?:$|[^b])|collaborators|invitations|keys|hooks|actions/(secrets|variables|permissions|runners)|dependabot/secrets|environments|pages|teams|transfer|vulnerability-alerts|automated-security-fixes|private-vulnerability-reporting|autolinks|interaction-limits|codespaces)`)
	tagRef     = regexp.MustCompile(`^refs/tags/v\d+\.\d+\.\d+$`)
	graphAllow = regexp.MustCompile(`^mutation\(\$id: ID!\) \{ (markPullRequestReadyForReview|pinIssue)\(input: \{(pullRequestId|issueId): \$id\}\) \{ [a-zA-Z]+ \{ number \} \} \}$`)
)

// Guard says why a request is refused ("" = allowed).
func (c *Client) Guard(method, path string, body any) string {
	path, _, _ = strings.Cut(path, "?")
	if c.anon && method != "GET" {
		return "an anonymous client only reads"
	}
	if path == "/graphql" {
		q, _ := body.(map[string]any)
		s, _ := q["query"].(string)
		if method == "POST" && graphAllow.MatchString(s) {
			return ""
		}
		return "GraphQL beyond marking a PR ready and pinning an issue"
	}
	if path == "/user" && method == "GET" {
		return ""
	}
	prefix := "/repos/" + c.Repo
	if path == prefix {
		if method == "GET" {
			return ""
		}
		return "changing or deleting the repository's settings"
	}
	rest, ok := strings.CutPrefix(path, prefix+"/")
	if !ok {
		return "a path outside the repository"
	}
	rest = "/" + rest
	if adminDeny.MatchString(rest) {
		return "an administration endpoint"
	}
	if method == "GET" {
		return ""
	}
	for _, a := range writeAllow {
		if a.method == method && a.re.MatchString(rest) {
			if rest == "/git/refs" {
				b, _ := json.Marshal(body)
				var r struct{ Ref string }
				_ = json.Unmarshal(b, &r)
				if !tagRef.MatchString(r.Ref) {
					return "creating a ref other than a v* release tag"
				}
			}
			return ""
		}
	}
	return fmt.Sprintf("%s %s is not among the writes Warden makes", method, rest)
}

// AllowedPush: a git push refspec Warden may use — only to warden/* branches.
func AllowedPush(refspec string) bool {
	_, dst, ok := strings.Cut(strings.TrimPrefix(refspec, "+"), ":")
	return ok && strings.HasPrefix(dst, "refs/heads/warden/") && !strings.Contains(dst, "..")
}
