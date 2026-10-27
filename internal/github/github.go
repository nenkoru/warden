// Package github is a small GitHub REST client for one repository, with the
// token read from a file. The token goes only into the Authorization header
// of requests to the configured API base: never into a URL, an argv, an
// error, a log line or a return value. Redact strips it from any text that
// leaves Warden anyway (comments, reports, git output), as a second line.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nenkoru/warden/internal/version"
)

// Client talks to one repository.
type Client struct {
	Base string // https://api.github.com (tests: an httptest server)
	Repo string // owner/name
	HTTP *http.Client

	token string
	mu    sync.Mutex
	login string
}

// ErrNotFound: a 404.
var ErrNotFound = errors.New("not found")

// New reads the token file (one line; surrounding space trimmed). A token
// file readable by others (or writable by its group) is refused: it is a
// credential. Group-read is allowed: a Kubernetes secret mounted with
// fsGroup is 0440.
func New(base, repo, tokenFile string) (*Client, error) {
	if !validRepo(repo) {
		return nil, fmt.Errorf("github: repo must be owner/name, not %q", repo)
	}
	if tokenFile == "" {
		return nil, errors.New("github: params.token_file is not set")
	}
	st, err := os.Stat(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("github: token file: %w", err)
	}
	if st.Mode().Perm()&0o027 != 0 {
		return nil, fmt.Errorf("github: token file %s is readable by others (chmod 600 it)", filepath.Base(tokenFile))
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("github: token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " \n\r\t") {
		return nil, errors.New("github: the token file must hold one token")
	}
	if base == "" {
		base = "https://api.github.com"
	}
	return &Client{Base: strings.TrimRight(base, "/"), Repo: repo, HTTP: &http.Client{Timeout: 60 * time.Second}, token: tok}, nil
}

func validRepo(r string) bool {
	o, n, cut := strings.Cut(r, "/")
	for _, c := range r {
		ok := c == '/' || c == '-' || c == '_' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !ok {
			return false
		}
	}
	return cut && o != "" && n != "" && !strings.Contains(n, "/")
}

// Redact replaces the token in s (and any line that looks like a credential
// helper's password line).
func (c *Client) Redact(s string) string {
	if c == nil || c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "[REDACTED]")
}

// Contains: whether s holds the token (a change, a comment Warden is about to publish).
func (c *Client) Contains(s string) bool {
	return c != nil && c.token != "" && strings.Contains(s, c.token)
}

func (c *Client) repoPath(format string, a ...any) string {
	return "/repos/" + c.Repo + fmt.Sprintf(format, a...)
}

// do sends one request; out may be nil. Errors name the method, the path and
// the status, and a short piece of the body — redacted.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.send(ctx, method, path, "application/vnd.github+json", body, out)
}

// raw: a GET whose answer is text (a diff, a log).
func (c *Client) raw(ctx context.Context, path, accept string) (string, error) {
	var b []byte
	err := c.send(ctx, "GET", path, accept, nil, &b)
	return c.Redact(string(b)), err
}

func (c *Client) send(ctx context.Context, method, path, accept string, body, out any) error {
	if why := c.Guard(method, path, body); why != "" {
		return fmt.Errorf("github %s %s: refused by Warden's guard: %s", method, path, why)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", version.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL carries no secret, but keep errors short
		}
		return fmt.Errorf("github %s %s: %s", method, path, c.Redact(err.Error()))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	if resp.StatusCode == 404 {
		return fmt.Errorf("github %s %s: %w", method, path, ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		var e struct{ Message string }
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode, Message: c.Redact(msg)}
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = b
		return nil
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("github %s %s: %w", method, path, err)
		}
	}
	return nil
}

// StatusError: a non-2xx answer.
type StatusError struct {
	Method, Path string
	Code         int
	Message      string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Code, e.Message)
}

// ---- types (only the fields Warden reads)

type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type Label struct {
	Name string `json:"name"`
}

type Issue struct {
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	State             string    `json:"state"`
	User              User      `json:"user"`
	AuthorAssociation string    `json:"author_association"`
	Labels            []Label   `json:"labels"`
	HTMLURL           string    `json:"html_url"`
	NodeID            string    `json:"node_id"`
	PullRequest       *struct{} `json:"pull_request,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

func (i Issue) Has(label string) bool { return hasLabel(i.Labels, label) }

type Ref struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type PR struct {
	Number            int        `json:"number"`
	NodeID            string     `json:"node_id"`
	Title             string     `json:"title"`
	Body              string     `json:"body"`
	State             string     `json:"state"`
	Draft             bool       `json:"draft"`
	User              User       `json:"user"`
	AuthorAssociation string     `json:"author_association"`
	Labels            []Label    `json:"labels"`
	Head              Ref        `json:"head"`
	Base              Ref        `json:"base"`
	HTMLURL           string     `json:"html_url"`
	Mergeable         *bool      `json:"mergeable"`
	MergeableState    string     `json:"mergeable_state"`
	Merged            bool       `json:"merged"`
	MergedAt          *time.Time `json:"merged_at"`
	MergeCommitSHA    string     `json:"merge_commit_sha"`
}

func (p PR) Has(label string) bool { return hasLabel(p.Labels, label) }

func hasLabel(ls []Label, name string) bool {
	for _, l := range ls {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// LabelNames: the labels, lower-cased.
func LabelNames(ls []Label) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, strings.ToLower(l.Name))
	}
	return out
}

type File struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
}

type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion"` // success | failure | neutral | skipped | timed_out | action_required
}

type WorkflowRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	HeadBranch string    `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	CreatedAt  time.Time `json:"created_at"`
	WorkflowID int64     `json:"workflow_id"`
}

type Tag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User User   `json:"user"`
}

// ---- calls

// Login: the token's user (cached).
func (c *Client) Login(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.login != "" {
		return c.login, nil
	}
	var u User
	if err := c.do(ctx, "GET", "/user", nil, &u); err != nil {
		return "", err
	}
	c.login = u.Login
	return u.Login, nil
}

// OpenIssues: open issues, pull requests left out.
func (c *Client) OpenIssues(ctx context.Context) ([]Issue, error) {
	var all []Issue
	for page := 1; page <= 10; page++ {
		var is []Issue
		if err := c.do(ctx, "GET", c.repoPath("/issues?state=open&per_page=100&page=%d", page), nil, &is); err != nil {
			return nil, err
		}
		for _, i := range is {
			if i.PullRequest == nil {
				all = append(all, i)
			}
		}
		if len(is) < 100 {
			break
		}
	}
	return all, nil
}

// IssuesLabeled: open issues (not PRs) with a label.
func (c *Client) IssuesLabeled(ctx context.Context, label string) ([]Issue, error) {
	var is []Issue
	if err := c.do(ctx, "GET", c.repoPath("/issues?state=open&per_page=100&labels=%s", url.QueryEscape(label)), nil, &is); err != nil {
		return nil, err
	}
	var out []Issue
	for _, i := range is {
		if i.PullRequest == nil {
			out = append(out, i)
		}
	}
	return out, nil
}

func (c *Client) Issue(ctx context.Context, n int) (Issue, error) {
	var i Issue
	return i, c.do(ctx, "GET", c.repoPath("/issues/%d", n), nil, &i)
}

func (c *Client) OpenPRs(ctx context.Context) ([]PR, error) {
	var ps []PR
	return ps, c.do(ctx, "GET", c.repoPath("/pulls?state=open&per_page=100"), nil, &ps)
}

// ClosedPRs: recently updated closed PRs into base.
func (c *Client) ClosedPRs(ctx context.Context, base string) ([]PR, error) {
	var ps []PR
	return ps, c.do(ctx, "GET", c.repoPath("/pulls?state=closed&base=%s&sort=updated&direction=desc&per_page=100", url.QueryEscape(base)), nil, &ps)
}

func (c *Client) PR(ctx context.Context, n int) (PR, error) {
	var p PR
	return p, c.do(ctx, "GET", c.repoPath("/pulls/%d", n), nil, &p)
}

func (c *Client) PRFiles(ctx context.Context, n int) ([]File, error) {
	var all []File
	for page := 1; page <= 30; page++ {
		var fs []File
		if err := c.do(ctx, "GET", c.repoPath("/pulls/%d/files?per_page=100&page=%d", n, page), nil, &fs); err != nil {
			return nil, err
		}
		all = append(all, fs...)
		if len(fs) < 100 {
			break
		}
	}
	return all, nil
}

func (c *Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var r struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	return r.CheckRuns, c.do(ctx, "GET", c.repoPath("/commits/%s/check-runs?per_page=100", url.PathEscape(sha)), nil, &r)
}

// CombinedState: the commit statuses' combined state (success | pending |
// failure), and how many statuses there are.
func (c *Client) CombinedState(ctx context.Context, sha string) (string, int, error) {
	var r struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
	}
	err := c.do(ctx, "GET", c.repoPath("/commits/%s/status", url.PathEscape(sha)), nil, &r)
	return r.State, r.TotalCount, err
}

// Runs: workflow runs for a branch (or a tag name), newest first.
func (c *Client) Runs(ctx context.Context, branch string) ([]WorkflowRun, error) {
	var r struct {
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	return r.WorkflowRuns, c.do(ctx, "GET", c.repoPath("/actions/runs?branch=%s&per_page=30", url.QueryEscape(branch)), nil, &r)
}

// BranchSHA: the head commit of a branch.
func (c *Client) BranchSHA(ctx context.Context, branch string) (string, error) {
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	return r.Object.SHA, c.do(ctx, "GET", c.repoPath("/git/ref/heads/%s", branch), nil, &r)
}

func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	var ts []Tag
	return ts, c.do(ctx, "GET", c.repoPath("/tags?per_page=100"), nil, &ts)
}

// CommitDate: the committer date of a commit.
func (c *Client) CommitDate(ctx context.Context, sha string) (time.Time, error) {
	var r struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	err := c.do(ctx, "GET", c.repoPath("/commits/%s", url.PathEscape(sha)), nil, &r)
	return r.Commit.Committer.Date, err
}

// FileExists: whether path exists at ref (the contents API).
func (c *Client) FileExists(ctx context.Context, path, ref string) (bool, error) {
	err := c.do(ctx, "GET", c.repoPath("/contents/%s?ref=%s", path, url.QueryEscape(ref)), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) Comment(ctx context.Context, n int, body string) error {
	return c.do(ctx, "POST", c.repoPath("/issues/%d/comments", n), map[string]string{"body": c.Redact(body)}, nil)
}

func (c *Client) Comments(ctx context.Context, n int) ([]Comment, error) {
	var cs []Comment
	return cs, c.do(ctx, "GET", c.repoPath("/issues/%d/comments?per_page=100", n), nil, &cs)
}

func (c *Client) AddLabels(ctx context.Context, n int, labels ...string) error {
	return c.do(ctx, "POST", c.repoPath("/issues/%d/labels", n), map[string][]string{"labels": labels}, nil)
}

// Review posts a pull request review. The only event Warden sends is
// COMMENT: it never approves and never requests changes.
func (c *Client) Review(ctx context.Context, n int, sha, body string) error {
	return c.do(ctx, "POST", c.repoPath("/pulls/%d/reviews", n), map[string]string{"commit_id": sha, "body": c.Redact(body), "event": "COMMENT"}, nil)
}

// Merge: see kernel.go (the merge re-checks the kernel there).

func (c *Client) CreatePR(ctx context.Context, head, base, title, body string, draft bool) (PR, error) {
	var p PR
	return p, c.do(ctx, "POST", c.repoPath("/pulls"), map[string]any{"head": head, "base": base, "title": title, "body": c.Redact(body), "draft": draft}, &p)
}

// ClosePR closes a pull request without merging it.
func (c *Client) ClosePR(ctx context.Context, n int) error {
	return c.do(ctx, "PATCH", c.repoPath("/pulls/%d", n), map[string]string{"state": "closed"}, nil)
}

// MarkReady takes a draft PR out of draft (GraphQL: REST cannot).
func (c *Client) MarkReady(ctx context.Context, nodeID string) error {
	q := map[string]any{"query": "mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { number } } }", "variables": map[string]string{"id": nodeID}}
	var r struct {
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := c.do(ctx, "POST", "/graphql", q, &r); err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("github graphql: %s", c.Redact(r.Errors[0].Message))
	}
	return nil
}

// Diff: a PR's unified diff (never its code checked out).
func (c *Client) Diff(ctx context.Context, n int) (string, error) {
	return c.raw(ctx, c.repoPath("/pulls/%d", n), "application/vnd.github.diff")
}

// Branch: a branch and its head commit.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

func (c *Client) Branches(ctx context.Context) ([]Branch, error) {
	var bs []Branch
	return bs, c.do(ctx, "GET", c.repoPath("/branches?per_page=100"), nil, &bs)
}

// Commit: a commit of a comparison, its sha and full message.
type Commit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// Compare: the commits head has that base does not, oldest first (at most
// 250, the API's page), and how many there are in all.
func (c *Client) Compare(ctx context.Context, base, head string) ([]Commit, int, error) {
	var r struct {
		AheadBy int      `json:"ahead_by"`
		Commits []Commit `json:"commits"`
	}
	err := c.do(ctx, "GET", c.repoPath("/compare/%s...%s", escRef(base), escRef(head)), nil, &r)
	return r.Commits, r.AheadBy, err
}

// escRef: a branch name for a path, its slashes kept.
func escRef(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// FailedLogs: the logs of the failed jobs of the workflow runs on sha, the
// last max bytes of each (text for a model: the caller fences it).
func (c *Client) FailedLogs(ctx context.Context, sha string, max int) (string, error) {
	var runs struct {
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	if err := c.do(ctx, "GET", c.repoPath("/actions/runs?head_sha=%s&per_page=30", url.QueryEscape(sha)), nil, &runs); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range runs.WorkflowRuns {
		if r.Conclusion != "failure" && r.Conclusion != "timed_out" {
			continue
		}
		var jobs struct {
			Jobs []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"jobs"`
		}
		if err := c.do(ctx, "GET", c.repoPath("/actions/runs/%d/jobs?per_page=100", r.ID), nil, &jobs); err != nil {
			return b.String(), err
		}
		for _, j := range jobs.Jobs {
			if j.Conclusion != "failure" && j.Conclusion != "timed_out" {
				continue
			}
			log, err := c.raw(ctx, c.repoPath("/actions/jobs/%d/logs", j.ID), "application/vnd.github+json")
			if err != nil {
				log = "(log unavailable: " + err.Error() + ")"
			}
			if len(log) > max {
				log = "…" + log[len(log)-max:]
			}
			fmt.Fprintf(&b, "### %s / %s (%s)\n%s\n", r.Name, j.Name, j.Conclusion, log)
		}
	}
	return b.String(), nil
}

func (c *Client) CreateIssue(ctx context.Context, title, body string, labels ...string) (Issue, error) {
	var i Issue
	return i, c.do(ctx, "POST", c.repoPath("/issues"), map[string]any{"title": title, "body": c.Redact(body), "labels": labels}, &i)
}

// CreateTag: a lightweight tag refs/tags/name on sha.
func (c *Client) CreateTag(ctx context.Context, name, sha string) error {
	return c.do(ctx, "POST", c.repoPath("/git/refs"), map[string]string{"ref": "refs/tags/" + name, "sha": sha}, nil)
}

// Pin pins an issue (GraphQL; best effort: the caller may ignore the error).
func (c *Client) Pin(ctx context.Context, nodeID string) error {
	q := map[string]any{"query": "mutation($id: ID!) { pinIssue(input: {issueId: $id}) { issue { number } } }", "variables": map[string]string{"id": nodeID}}
	return c.do(ctx, "POST", "/graphql", q, nil)
}

// Number parses an issue/PR number.
func Number(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not an issue number: %q", s)
	}
	return n, nil
}

// CredentialHelper: a git credential helper (for `-c credential.helper=…`)
// that hands git the token from the file at the path in the environment
// variable WARDEN_GIT_TOKEN_FILE. The helper text names the variable, never
// the token; the token reaches git only through the helper's stdout pipe.
const CredentialHelper = `!f() { test "$1" = get || exit 0; echo username=x-access-token; printf 'password='; tr -d ' \n\r\t' < "$WARDEN_GIT_TOKEN_FILE"; echo; }; f`
