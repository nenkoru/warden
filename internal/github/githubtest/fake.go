// Package githubtest is an in-memory fake of the parts of the GitHub REST
// API Warden uses, for offline tests: issues, labels, comments, pull
// requests (files, diff, reviews, merge), check runs and statuses, workflow
// runs, jobs and logs, commits (dates, parents), refs, tags, branches,
// comparisons, contents, releases and their assets, attestations and two
// GraphQL mutations. Every request must carry the token (unless Anonymous
// allows reads without one); every request body is kept so a test can
// assert the token never left in one.
package githubtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/github"
)

// PR: a pull request and what the fake knows about it.
type PR struct {
	github.PR
	Files []string
	Diff  string
	// MergeConflict: when true, the merge endpoint returns 405.
	MergeConflict bool
}

// Review: a posted review (User: its author; "" = the token's user).
type Review struct {
	N                      int
	SHA, Event, Body, User string
}

// Job: a workflow job and its log.
type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Log        string `json:"-"`
}

type Fake struct {
	Token, Login, Repo string
	Server             *httptest.Server
	Now                time.Time

	mu       sync.Mutex
	Issues   map[int]*github.Issue
	PRs      map[int]*PR
	Checks   map[string][]github.CheckRun // sha → runs
	Status   map[string]string            // sha → combined state
	Runs     []github.WorkflowRun
	Jobs     map[int64][]Job
	Tags     []github.Tag
	Dates    map[string]time.Time // commit sha → date
	Parents  map[string][]string  // commit sha → parent shas
	Branches map[string]string    // name → sha
	Files    map[string]bool      // paths present on main (PAUSE)
	Contents map[string]string    // "ref:path" or "path" (any ref) → the file's text (the contents API, raw)
	Releases []github.Release     // newest first, like GitHub (AddRelease prepends)
	Comments map[int][]string
	// CommentBy: comment authors by comment ID (default: Login). Comment IDs
	// are issue*1000 + index + 1.
	CommentBy map[int64]string
	// CommentAssoc: author associations by comment ID (default: OWNER for
	// Login, NONE for anyone else).
	CommentAssoc map[int64]string
	// CommentAt: updated_at by comment ID, set to Now on create and edit
	// (default: Now).
	CommentAt map[int64]time.Time
	Assets    map[int64][]byte                // asset ID → content
	Attests   map[string][]github.Attestation // "sha256:hex" → attestations
	Anonymous bool                            // GET requests without a token are served
	Reviews   []Review
	Merged    []int
	Refs      map[string]string // refs created through the API → sha
	Bodies    []string          // every request body
	Requests  []string          // "METHOD path"
	Ready     []int             // PRs marked ready for review
	next      int
	// ResolveHead: the sha of a pushed branch (tests: from the bare remote).
	ResolveHead func(branch string) string
	// FilesOf: the changed files of a pushed branch (tests: from the bare remote).
	FilesOf func(branch string) []string
	// CommitsOf: the commits of head not on base, oldest first (tests: from
	// the bare remote); nil answers every comparison 404.
	CommitsOf func(base, head string) []github.Commit
}

// New starts the fake; it closes with the test.
func New(t testing.TB, token, login, repo string) *Fake {
	f := &Fake{Token: token, Login: login, Repo: repo, Now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
		Issues: map[int]*github.Issue{}, PRs: map[int]*PR{}, Checks: map[string][]github.CheckRun{}, Status: map[string]string{},
		Jobs: map[int64][]Job{}, Dates: map[string]time.Time{}, Parents: map[string][]string{}, Branches: map[string]string{"main": "main0"}, Files: map[string]bool{},
		Contents: map[string]string{}, Comments: map[int][]string{}, Refs: map[string]string{}, CommentBy: map[int64]string{}, CommentAssoc: map[int64]string{},
		CommentAt: map[int64]time.Time{}, Assets: map[int64][]byte{}, Attests: map[string][]github.Attestation{}, next: 1}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *Fake) URL() string { return f.Server.URL }

func labels(names ...string) []github.Label {
	var out []github.Label
	for _, n := range names {
		out = append(out, github.Label{Name: n})
	}
	return out
}

// AddIssue adds an open issue and returns its number.
func (f *Fake) AddIssue(title, body, author, assoc string, ls ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.next
	f.next++
	f.Issues[n] = &github.Issue{Number: n, Title: title, Body: body, State: "open", User: github.User{Login: author}, AuthorAssociation: assoc,
		Labels: labels(ls...), HTMLURL: fmt.Sprintf("https://github.com/%s/issues/%d", f.Repo, n), NodeID: fmt.Sprintf("I_%d", n), CreatedAt: f.Now}
	return n
}

// AddPR adds an open PR with a head sha, its files and a green or red CI.
func (f *Fake) AddPR(title, author, ref, sha string, files []string, ci string, ls ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.next
	f.next++
	t := true
	p := &PR{PR: github.PR{Number: n, NodeID: fmt.Sprintf("PR_%d", n), Title: title, State: "open", User: github.User{Login: author}, Labels: labels(ls...),
		Head: github.Ref{Ref: ref, SHA: sha}, Base: github.Ref{Ref: "main"}, HTMLURL: fmt.Sprintf("https://github.com/%s/pull/%d", f.Repo, n),
		Mergeable: &t, MergeableState: "clean"}, Files: files, Diff: "diff --git a/x b/x\n+change\n"}
	f.PRs[n] = p
	f.setCI(sha, ci)
	return n
}

// SetCI sets a commit's check runs: green, red, pending or none.
func (f *Fake) SetCI(sha, ci string) { f.mu.Lock(); f.setCI(sha, ci); f.mu.Unlock() }

func (f *Fake) setCI(sha, ci string) {
	switch ci {
	case "green":
		f.Checks[sha] = []github.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}, {Name: "lint", Status: "completed", Conclusion: "success"}}
	case "red":
		f.Checks[sha] = []github.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}}
	case "pending":
		f.Checks[sha] = []github.CheckRun{{Name: "test", Status: "in_progress"}}
	default:
		delete(f.Checks, sha)
	}
}

// AddRun adds a workflow run.
func (f *Fake) AddRun(r github.WorkflowRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ID == 0 {
		r.ID = int64(len(f.Runs) + 100)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = f.Now.Add(time.Duration(len(f.Runs)) * time.Second)
	}
	f.Runs = append(f.Runs, r)
}

// MergedPR adds a closed, merged PR (for releases).
func (f *Fake) MergedPR(title, ref string, at time.Time, ls ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.next
	f.next++
	f.PRs[n] = &PR{PR: github.PR{Number: n, Title: title, State: "closed", Merged: true, MergedAt: &at, User: github.User{Login: "someone"},
		Head: github.Ref{Ref: ref, SHA: "h" + strconv.Itoa(n)}, Base: github.Ref{Ref: "main"}, Labels: labels(ls...), MergeCommitSHA: "m" + strconv.Itoa(n)}}
	return n
}

// AddComment adds a comment by author to issue n and returns its ID.
func (f *Fake) AddComment(n int, author, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Comments[n] = append(f.Comments[n], body)
	id := commentID(n, len(f.Comments[n])-1)
	f.CommentBy[id] = author
	f.CommentAt[id] = f.Now
	return id
}

func commentID(n, i int) int64 { return int64(n)*1000 + int64(i) + 1 }

// AddRelease adds a published release; assets maps a name to its content.
func (f *Fake) AddRelease(tag string, prerelease bool, published time.Time, assets map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := github.Release{ID: int64(len(f.Releases) + 1), TagName: tag, Prerelease: prerelease, PublishedAt: &published,
		HTMLURL: fmt.Sprintf("https://github.com/%s/releases/tag/%s", f.Repo, tag)}
	names := make([]string, 0, len(assets))
	for n := range assets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		id := int64(len(f.Assets) + 1000)
		f.Assets[id] = assets[n]
		r.Assets = append(r.Assets, github.Asset{ID: id, Name: n, Size: int64(len(assets[n]))})
	}
	f.Releases = append([]github.Release{r}, f.Releases...) // newest first, like GitHub
}

// With runs fn under the fake's lock (to inspect or change state).
func (f *Fake) With(fn func(f *Fake)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }

var (
	reIssue    = regexp.MustCompile(`^/issues/(\d+)$`)
	reIssueSub = regexp.MustCompile(`^/issues/(\d+)/(comments|labels)$`)
	reComment  = regexp.MustCompile(`^/issues/comments/(\d+)$`)
	reAsset    = regexp.MustCompile(`^/releases/assets/(\d+)$`)
	rePull     = regexp.MustCompile(`^/pulls/(\d+)$`)
	rePullSub  = regexp.MustCompile(`^/pulls/(\d+)/(files|merge|reviews)$`)
	reCommit   = regexp.MustCompile(`^/commits/([^/]+)(/check-runs|/status)?$`)
	reJobs     = regexp.MustCompile(`^/actions/runs/(\d+)/jobs$`)
	reLog      = regexp.MustCompile(`^/actions/jobs/(\d+)/logs$`)
)

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Bodies = append(f.Bodies, string(body))
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	anon := f.Anonymous && r.Method == "GET" && r.Header.Get("Authorization") == ""
	if r.Header.Get("Authorization") != "Bearer "+f.Token && !anon {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	js := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	if r.URL.Path == "/user" {
		js(github.User{Login: f.Login})
		return
	}
	if r.URL.Path == "/graphql" {
		q, _ := in["query"].(string)
		vars, _ := in["variables"].(map[string]any)
		id, _ := vars["id"].(string)
		if strings.Contains(q, "markPullRequestReadyForReview") {
			for _, p := range f.PRs {
				if p.NodeID == id {
					p.Draft = false
					f.Ready = append(f.Ready, p.Number)
				}
			}
		}
		js(map[string]any{"data": map[string]any{}})
		return
	}
	prefix := "/repos/" + f.Repo
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	qv := r.URL.Query()
	switch {
	case path == "/issues" && r.Method == "GET":
		var out []github.Issue
		for n := 1; n < f.next; n++ {
			if i := f.Issues[n]; i != nil && i.State == "open" && (qv.Get("labels") == "" || i.Has(qv.Get("labels"))) {
				out = append(out, *i)
			}
		}
		js(out)
	case path == "/issues" && r.Method == "POST":
		n := f.next
		f.next++
		i := &github.Issue{Number: n, Title: str(in["title"]), Body: str(in["body"]), State: "open", User: github.User{Login: f.Login},
			AuthorAssociation: "OWNER", NodeID: fmt.Sprintf("I_%d", n), HTMLURL: fmt.Sprintf("https://github.com/%s/issues/%d", f.Repo, n), CreatedAt: f.Now}
		if ls, ok := in["labels"].([]any); ok {
			for _, l := range ls {
				i.Labels = append(i.Labels, github.Label{Name: str(l)})
			}
		}
		f.Issues[n] = i
		w.WriteHeader(201)
		js(i)
	case reIssue.MatchString(path):
		n, _ := strconv.Atoi(reIssue.FindStringSubmatch(path)[1])
		if i := f.Issues[n]; i != nil {
			js(i)
			return
		}
		http.NotFound(w, r)
	case reIssueSub.MatchString(path):
		m := reIssueSub.FindStringSubmatch(path)
		n, _ := strconv.Atoi(m[1])
		switch {
		case m[2] == "comments" && r.Method == "POST":
			f.Comments[n] = append(f.Comments[n], str(in["body"]))
			f.CommentAt[commentID(n, len(f.Comments[n])-1)] = f.Now
			w.WriteHeader(201)
			js(map[string]any{"id": commentID(n, len(f.Comments[n])-1)})
		case m[2] == "comments":
			var out []github.Comment
			for i, c := range f.Comments[n] {
				out = append(out, f.comment(n, i, c))
			}
			js(out)
		case m[2] == "labels":
			ls, _ := in["labels"].([]any)
			for _, l := range ls {
				lab := github.Label{Name: str(l)}
				if i := f.Issues[n]; i != nil && !i.Has(lab.Name) {
					i.Labels = append(i.Labels, lab)
				}
				if p := f.PRs[n]; p != nil && !p.Has(lab.Name) {
					p.Labels = append(p.Labels, lab)
				}
			}
			js([]any{})
		}
	case reComment.MatchString(path):
		id, _ := strconv.ParseInt(reComment.FindStringSubmatch(path)[1], 10, 64)
		n, i := int(id/1000), int(id%1000)-1
		if i < 0 || i >= len(f.Comments[n]) {
			http.NotFound(w, r)
			return
		}
		if r.Method == "PATCH" {
			f.Comments[n][i] = str(in["body"])
			f.CommentAt[id] = f.Now
		}
		js(f.comment(n, i, f.Comments[n][i]))
	case path == "/releases":
		js(f.Releases)
	case reAsset.MatchString(path):
		id, _ := strconv.ParseInt(reAsset.FindStringSubmatch(path)[1], 10, 64)
		b, ok := f.Assets[id]
		if !ok || r.Header.Get("Accept") != "application/octet-stream" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	case strings.HasPrefix(path, "/attestations/"):
		as, ok := f.Attests[strings.TrimPrefix(path, "/attestations/")]
		if !ok {
			w.WriteHeader(404)
			js(map[string]string{"message": "Not Found"})
			return
		}
		js(map[string]any{"attestations": as})
	case path == "/pulls" && r.Method == "GET":
		out := []github.PR{}
		for n := 1; n < f.next; n++ {
			if p := f.PRs[n]; p != nil && p.State == qv.Get("state") {
				out = append(out, p.PR)
			}
		}
		js(out)
	case path == "/pulls" && r.Method == "POST":
		head := str(in["head"])
		for _, p := range f.PRs {
			if p.State == "open" && p.Head.Ref == head {
				w.WriteHeader(422)
				js(map[string]string{"message": "A pull request already exists"})
				return
			}
		}
		sha := "sha-" + head
		if f.ResolveHead != nil {
			sha = f.ResolveHead(head)
		}
		var files []string
		if f.FilesOf != nil {
			files = f.FilesOf(head)
		}
		n := f.next
		f.next++
		t := true
		draft, _ := in["draft"].(bool)
		p := &PR{PR: github.PR{Number: n, NodeID: fmt.Sprintf("PR_%d", n), Title: str(in["title"]), Body: str(in["body"]), State: "open", Draft: draft,
			User: github.User{Login: f.Login}, Head: github.Ref{Ref: head, SHA: sha}, Base: github.Ref{Ref: str(in["base"])}, Mergeable: &t, MergeableState: "clean",
			HTMLURL: fmt.Sprintf("https://github.com/%s/pull/%d", f.Repo, n)}, Files: files}
		f.PRs[n] = p
		w.WriteHeader(201)
		js(p.PR)
	case rePull.MatchString(path):
		n, _ := strconv.Atoi(rePull.FindStringSubmatch(path)[1])
		p := f.PRs[n]
		if p == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == "PATCH" {
			if s := str(in["state"]); s != "" {
				p.State = s
			}
		}
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			_, _ = io.WriteString(w, p.Diff)
			return
		}
		js(p.PR)
	case rePullSub.MatchString(path):
		m := rePullSub.FindStringSubmatch(path)
		n, _ := strconv.Atoi(m[1])
		p := f.PRs[n]
		if p == nil {
			http.NotFound(w, r)
			return
		}
		switch m[2] {
		case "files":
			var out []github.File
			if qv.Get("page") == "1" || qv.Get("page") == "" {
				for _, fl := range p.Files {
					out = append(out, github.File{Filename: fl, Status: "modified"})
				}
			}
			js(out)
		case "reviews":
			if r.Method == "GET" {
				out := []github.PRReview{}
				for i, rv := range f.Reviews {
					if rv.N == n {
						out = append(out, github.PRReview{ID: int64(i + 1), User: github.User{Login: or(rv.User, f.Login)}, State: "COMMENTED", Body: rv.Body})
					}
				}
				js(out)
				return
			}
			f.Reviews = append(f.Reviews, Review{N: n, SHA: str(in["commit_id"]), Event: str(in["event"]), Body: str(in["body"])})
			js(map[string]any{"id": len(f.Reviews)})
		case "merge":
			if p.MergeConflict {
				w.WriteHeader(405)
				js(map[string]string{"message": "Merge conflict"})
				return
			}
			if p.State != "open" || str(in["sha"]) != p.Head.SHA {
				w.WriteHeader(409)
				js(map[string]string{"message": "Head branch was modified"})
				return
			}
			now := f.Now
			p.State, p.Merged, p.MergedAt, p.MergeCommitSHA = "closed", true, &now, "m"+strconv.Itoa(n)
			p.MergedBy = &github.User{Login: f.Login}
			f.Merged = append(f.Merged, n)
			f.Branches["main"] = p.MergeCommitSHA
			js(map[string]any{"merged": true, "sha": p.MergeCommitSHA})
		}
	case reCommit.MatchString(path):
		m := reCommit.FindStringSubmatch(path)
		switch m[2] {
		case "/check-runs":
			js(map[string]any{"total_count": len(f.Checks[m[1]]), "check_runs": f.Checks[m[1]]})
		case "/status":
			st := f.Status[m[1]]
			n := 0
			if st != "" {
				n = 1
			} else {
				st = "pending"
			}
			js(map[string]any{"state": st, "total_count": n})
		default:
			d := f.Dates[m[1]]
			parents := []map[string]string{}
			for _, p := range f.Parents[m[1]] {
				parents = append(parents, map[string]string{"sha": p})
			}
			js(map[string]any{"sha": m[1], "commit": map[string]any{"committer": map[string]any{"date": d}}, "parents": parents})
		}
	case path == "/actions/runs":
		var out []github.WorkflowRun
		for i := len(f.Runs) - 1; i >= 0; i-- {
			rn := f.Runs[i]
			if b := qv.Get("branch"); b != "" && rn.HeadBranch != b {
				continue
			}
			if s := qv.Get("head_sha"); s != "" && rn.HeadSHA != s {
				continue
			}
			out = append(out, rn)
		}
		js(map[string]any{"workflow_runs": out})
	case reJobs.MatchString(path):
		id, _ := strconv.ParseInt(reJobs.FindStringSubmatch(path)[1], 10, 64)
		js(map[string]any{"jobs": f.Jobs[id]})
	case reLog.MatchString(path):
		id, _ := strconv.ParseInt(reLog.FindStringSubmatch(path)[1], 10, 64)
		for _, js := range f.Jobs {
			for _, j := range js {
				if j.ID == id {
					_, _ = io.WriteString(w, j.Log)
					return
				}
			}
		}
		http.NotFound(w, r)
	case strings.HasPrefix(path, "/git/ref/heads/"):
		b := strings.TrimPrefix(path, "/git/ref/heads/")
		if sha, ok := f.Branches[b]; ok {
			js(map[string]any{"ref": "refs/heads/" + b, "object": map[string]string{"sha": sha}})
			return
		}
		http.NotFound(w, r)
	case path == "/git/refs" && r.Method == "POST":
		ref := str(in["ref"])
		if _, ok := f.Refs[ref]; ok {
			w.WriteHeader(422)
			js(map[string]string{"message": "Reference already exists"})
			return
		}
		f.Refs[ref] = str(in["sha"])
		if name, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
			tg := github.Tag{Name: name}
			tg.Commit.SHA = str(in["sha"])
			f.Tags = append(f.Tags, tg)
			f.Dates[str(in["sha"])] = f.Now
		}
		w.WriteHeader(201)
		js(map[string]string{"ref": ref})
	case strings.HasPrefix(path, "/compare/") && r.Method == "GET":
		base, head, ok := strings.Cut(strings.TrimPrefix(path, "/compare/"), "...")
		if !ok || f.CommitsOf == nil {
			w.WriteHeader(404)
			js(map[string]string{"message": "Not Found"})
			return
		}
		cs := f.CommitsOf(base, head)
		js(map[string]any{"ahead_by": len(cs), "commits": cs})
	case path == "/tags":
		js(f.Tags)
	case path == "/branches":
		var out []github.Branch
		for name, sha := range f.Branches {
			b := github.Branch{Name: name}
			b.Commit.SHA = sha
			out = append(out, b)
		}
		js(out)
	case path == "/releases/latest":
		for _, rl := range f.Releases { // newest first
			if !rl.Draft && !rl.Prerelease {
				js(rl)
				return
			}
		}
		w.WriteHeader(404)
		js(map[string]string{"message": "Not Found"})
	case strings.HasPrefix(path, "/contents/") && strings.Contains(r.Header.Get("Accept"), "raw"):
		p := strings.TrimPrefix(path, "/contents/")
		c, ok := f.Contents[qv.Get("ref")+":"+p]
		if !ok {
			c, ok = f.Contents[p]
		}
		if ok {
			_, _ = io.WriteString(w, c)
			return
		}
		w.WriteHeader(404)
		js(map[string]string{"message": "Not Found"})
	case strings.HasPrefix(path, "/contents/"):
		p := strings.TrimPrefix(path, "/contents/")
		if _, ok := f.Contents[p]; ok || f.Files[p] {
			js(map[string]string{"type": "file"})
			return
		}
		w.WriteHeader(404)
		js(map[string]string{"message": "Not Found"})
	default:
		w.WriteHeader(404)
		js(map[string]string{"message": "Not Found: " + path})
	}
}

func (f *Fake) comment(n, i int, body string) github.Comment {
	id := commentID(n, i)
	by := f.CommentBy[id]
	if by == "" {
		by = f.Login
	}
	assoc := "NONE"
	if by == f.Login {
		assoc = "OWNER"
	}
	if a := f.CommentAssoc[id]; a != "" {
		assoc = a
	}
	at, ok := f.CommentAt[id]
	if !ok {
		at = f.Now
	}
	return github.Comment{ID: id, Body: body, User: github.User{Login: by}, AuthorAssociation: assoc, CreatedAt: at, UpdatedAt: at}
}

func str(v any) string { s, _ := v.(string); return s }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
