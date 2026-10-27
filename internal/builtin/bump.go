package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/policy"
)

// ---- bumps: a vulnerable dependency, fixed never in the owner's checkout.
// A fresh worktree from HEAD on a new branch warden/bump-…, under the state
// dir; the ecosystem's update (network: its registry only), a build where the
// ecosystem has one, the domain's checks (no network). Red → the brain may
// fix it in the same worktree; still red → the branch is dropped. Then OSV is
// asked again, one commit, a PR description next to it. Never pushes, never
// merges. The same playbook for every ecosystem; a bumper says how.

type step struct {
	argv []string
	net  []string // hosts; nil: no network
}

type bumper struct {
	eco, act string
	name     *regexp.Regexp // what a package name may look like (no flags, no paths)
	version  *regexp.Regexp
	file     string // the manifest the brain must not change the version in
	// update: files edited directly, then commands (in the worktree).
	update func(wt, state, mod, to string) ([]step, error)
	// build: compiles (or installs) before the checks; a failure goes to the brain.
	build func(wt, state string) []step
	// prepare: before the commit (drop what the bump made that is not the project's).
	prepare func(wt string)
}

var (
	goHosts   = []string{"proxy.golang.org", "sum.golang.org"}
	npmHosts  = []string{"registry.npmjs.org"}
	pypiHosts = []string{"pypi.org", "files.pythonhosted.org"}
	unsafeRef = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
	reModule  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._~/-]*$`)
	reVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*$`)
	reNpmName = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]*$`)
	reSemver  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*$`)
	rePyName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	rePyVer   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*[0-9A-Za-z.+!-]*$`)
)

var goBumper = bumper{eco: "Go", act: "go-bump", name: reModule, version: reVersion, file: "go.mod",
	update: func(wt, state, mod, to string) ([]step, error) {
		return []step{{[]string{"go", "get", mod + "@" + to}, goHosts}, {[]string{"go", "mod", "tidy"}, goHosts}}, nil
	},
	// packages and tests compile (no binary lands in the worktree): a fixed
	// version that changed its API fails here
	build: func(wt, state string) []step {
		return []step{{[]string{"go", "test", "-count=1", "-run=^$", "./..."}, nil}}
	},
}

// npm: a direct dependency through `npm install pkg@to`; a transitive one
// through an override in package.json (npm ≥ 8.3), then `npm install`.
// Lifecycle scripts never run (--ignore-scripts); the cache is Warden's.
var npmBumper = bumper{eco: "npm", act: "npm-bump", name: reNpmName, version: reSemver, file: "package.json",
	update: func(wt, state, mod, to string) ([]step, error) {
		var pkg map[string]json.RawMessage
		b, err := os.ReadFile(filepath.Join(wt, "package.json"))
		if err != nil {
			return nil, fmt.Errorf("package.json: %w", err)
		}
		if err := json.Unmarshal(b, &pkg); err != nil {
			return nil, fmt.Errorf("package.json: %w", err)
		}
		flags := []string{"--ignore-scripts", "--no-audit", "--no-fund", "--cache", filepath.Join(state, "work", "tmp", "npm-cache")}
		for _, sec := range []string{"dependencies", "devDependencies", "optionalDependencies"} {
			var deps map[string]string
			_ = json.Unmarshal(pkg[sec], &deps) // a missing or odd section is just empty
			if _, ok := deps[mod]; ok {
				return []step{{append([]string{"npm", "install"}, append(flags, mod+"@"+to)...), npmHosts}}, nil
			}
		}
		return []step{
			{[]string{"npm", "pkg", "set", "overrides[" + mod + "]=" + to}, nil},
			{append([]string{"npm", "install"}, flags...), npmHosts},
		}, nil
	},
}

// PyPI: the == pin in requirements.txt, edited in place; then a fresh venv
// in the worktree (.warden-venv, removed before the commit) with the pinned
// set installed, so the checks run against the new version.
var pipBumper = bumper{eco: "PyPI", act: "pip-bump", name: rePyName, version: rePyVer, file: "requirements.txt",
	update: func(wt, state, mod, to string) ([]step, error) {
		p := filepath.Join(wt, "requirements.txt")
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("requirements.txt: %w", err)
		}
		lines, n := strings.Split(string(b), "\n"), 0
		for i, l := range lines {
			spec, rest, _ := strings.Cut(l, ";")
			name, _, ok := strings.Cut(spec, "==")
			bare, _, _ := strings.Cut(name, "[")
			if ok && pyNorm(bare) == pyNorm(mod) {
				lines[i] = name + "==" + to
				if rest != "" {
					lines[i] += " ;" + rest
				}
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("requirements.txt has no %s== pin", mod)
		}
		return nil, os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
	},
	build: func(wt, state string) []step {
		py := filepath.Join(".warden-venv", "bin", "python")
		return []step{
			{[]string{"python3", "-m", "venv", ".warden-venv"}, nil},
			{[]string{py, "-m", "pip", "install", "-q", "--no-cache-dir", "--disable-pip-version-check", "-r", "requirements.txt"}, pypiHosts},
		}
	},
	prepare: func(wt string) { os.RemoveAll(filepath.Join(wt, ".warden-venv")) },
}

// pyNorm: PEP 503 name normalization.
func pyNorm(s string) string {
	return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(strings.TrimSpace(s), "-"))
}

func bump(ctx context.Context, with map[string]any, f domain.Finding, env *Env, bp bumper) Result {
	if f.Fix == nil {
		return Result{Summary: bp.act + ": the finding has no fix"}
	}
	p := f.Fix.Params
	mod, from, to := p["module"], p["from"], p["to"]
	switch {
	case p["ecosystem"] != "" && p["ecosystem"] != bp.eco:
		return Result{Summary: fmt.Sprintf("%s: not a %s package: %s", bp.act, bp.eco, p["ecosystem"])}
	case !bp.name.MatchString(mod) || !bp.version.MatchString(to):
		return Result{Summary: fmt.Sprintf("%s: refused: bad package or version %q %q", bp.act, mod, to)}
	case env.State == "":
		return Result{Summary: bp.act + ": no state dir"}
	}
	root, err := filepath.Abs(param(with, env, "root", "."))
	if err != nil {
		return Result{Summary: bp.act + ": " + err.Error()}
	}
	state, _ := filepath.Abs(env.State)
	branch := "warden/bump-" + strings.Trim(unsafeRef.ReplaceAllString(mod, "-"), "-") + "-" + to
	res := Result{Branch: branch}
	logf := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		res.Log = append(res.Log, s)
		env.log(s)
	}
	if _, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		res.OK, res.Summary = true, "already: branch "+branch+" exists"
		return res
	}
	head, err := git(ctx, root, "rev-parse", "--short", "HEAD")
	if err != nil {
		res.Summary, res.Branch = "not a git repository with commits: "+err.Error(), ""
		return res
	}
	work := filepath.Join(state, "work")
	wt := filepath.Join(work, domain.ID("bump", mod, to))
	if err := os.MkdirAll(work, 0o700); err != nil {
		res.Summary, res.Branch = err.Error(), ""
		return res
	}
	drop := func() { dropWorktree(ctx, root, work, wt) }
	drop() // a crashed earlier attempt
	if _, err := git(ctx, root, "worktree", "add", "-q", "-b", branch, wt, "HEAD"); err != nil {
		res.Summary, res.Branch = err.Error(), ""
		return res
	}
	defer drop()
	logf("worktree %s on new branch %s from %s", filepath.Base(wt), branch, head)

	fail := func(step, out string, err error) Result {
		res.Summary = fmt.Sprintf("%s failed: %v", step, err)
		if out != "" {
			logf("%s output:\n%s", step, tail(out, 3000))
		}
		drop()
		_, _ = git(ctx, root, "branch", "-D", branch) // only the branch this act just made
		res.Branch = ""
		return res
	}
	if env.Run == nil {
		return fail("run", "", errors.New("no runner"))
	}
	steps, err := bp.update(wt, state, mod, to)
	if err != nil {
		return fail("update", "", err)
	}
	if len(steps) == 0 {
		logf("%s: %s %s → %s", bp.file, mod, from, to)
	}
	for _, s := range steps {
		out, err := env.Run(ctx, wt, s.argv, policy.Floors{Sandbox: true, Net: s.net})
		if err != nil {
			return fail(strings.Join(s.argv[:min(2, len(s.argv))], " "), out, err)
		}
		logf("%s: ok", strings.Join(s.argv, " "))
	}
	// build (where the ecosystem has one), then the checks: both are the
	// brain's to fix when red
	verify := func(ctx context.Context) (string, error) {
		if bp.build != nil {
			for _, s := range bp.build(wt, state) {
				if out, err := env.Run(ctx, wt, s.argv, policy.Floors{Sandbox: true, Net: s.net}); err != nil {
					return out, err
				}
			}
		}
		return checks(ctx, env, wt)
	}
	out, err := verify(ctx)
	if err != nil && env.Brain != nil {
		logf("build or checks red after the bump; asking the brain to fix it in the worktree")
		task := fmt.Sprintf("The %s dependency %s was bumped from %s to %s to fix %s. The build or the checks now fail:\n\n```\n%s\n```\n\n"+
			"Make the smallest change that makes the checks pass (run_checks). Do not change the required version of %s in %s.",
			bp.eco, mod, from, to, p["advisories"], tail(out, 4000), mod, bp.file)
		note, berr := env.Brain.Do(brain.WithDir(ctx, wt), systemPrompt(fixerPrompt, env), task, append(fileTools(wt, env), env.Tools...))
		logf("brain: %s", firstLines(note, 6))
		if berr != nil {
			logf("brain: %v", berr)
		}
		out, err = verify(ctx)
	}
	if err != nil {
		return fail("checks", out, err)
	}
	logf("checks: ok")

	// the fixed version is what the lock file now says, everywhere
	var now []domain.Component
	for _, c := range lockfiles(wt) {
		if c.Ecosystem == bp.eco && (c.Name == mod || bp.eco == "PyPI" && pyNorm(c.Name) == pyNorm(mod)) {
			if c.Version != to && (bp.eco == "Go" || semverLess(c.Version, to)) {
				return fail("verify", "", fmt.Errorf("%s still has %s %s after the bump", bp.file, mod, c.Version))
			}
			now = append(now, c)
		}
	}
	if len(now) == 0 {
		return fail("verify", "", fmt.Errorf("%s does not list %s after the bump", bp.file, mod))
	}
	if left, err := Advisories(ctx, now, nil, env); err != nil {
		logf("verify: OSV unreachable (%v); the advisories' fixed ranges say %s is fixed", err, to)
	} else if len(left) > 0 {
		logf("verify: OSV still lists for %s: %s", to, left[0].Title)
	} else {
		logf("verify: OSV lists no vulnerabilities for %s %s", mod, to)
	}

	if bp.prepare != nil {
		bp.prepare(wt)
	}
	msg := fmt.Sprintf("deps: bump %s %s → %s\n\nFixes %s.\nOpened by Warden; checks passed.\n", mod, from, to, p["advisories"])
	if _, err := git(ctx, wt, "add", "-A"); err != nil {
		return fail("git add", "", err)
	}
	if _, err := git(ctx, wt, "commit", "-q", "--no-verify", "-m", msg); err != nil {
		return fail("git commit", "", err)
	}
	sha, _ := git(ctx, wt, "rev-parse", "--short", "HEAD")
	diff, _ := git(ctx, wt, "diff", "--stat", "HEAD~1")
	logf("commit %s on %s", sha, branch)

	pr := filepath.Join(state, "prs", strings.ReplaceAll(branch, "/", "_")+".md")
	body := fmt.Sprintf("# deps: bump %s %s → %s\n\nBranch: `%s` (commit %s, from %s). Not pushed.\n\n## Why\n\n%s\n\n%s\n\n"+
		"## What changed\n\n```\n%s\n```\n\n## Checks\n\n- %s\n\n## To merge\n\n```bash\ngit merge --ff-only %s\n```\n\n"+
		"or push the branch and open a PR. To drop it: `git branch -D %s`.\n",
		mod, from, to, branch, sha, head, f.Detail, strings.Join(f.Links, "\n"), diff, strings.Join(res.Log, "\n- "), branch, branch)
	if err := writeFile(pr, []byte(body)); err != nil {
		logf("PR description: %v", err)
	} else {
		res.File = pr
	}
	res.OK = true
	res.Summary = fmt.Sprintf("branch %s: %s %s → %s, checks pass", branch, mod, from, to)
	return res
}
