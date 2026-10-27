// Package box runs a command contained: sandbox-exec on macOS, bwrap on
// Linux, with egress only through an allowlist proxy.
package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

type Policy struct {
	Name                string
	ReadWrite, ReadOnly []string
	Net                 []string // hosts; nil = no network
	// Keychain (macOS): read the login keychain through securityd, for an
	// agent that logs in by reference (Claude Code's keychain item) instead
	// of a token in its environment. Nothing else under ~/Library opens.
	Keychain bool
}

type Result struct {
	Output  string   // stdout and stderr combined
	Runner  string   // sandbox-exec|bwrap|none
	Refused []string // egress refusals
}

// secretDirs are unreadable inside the box unless listed in ReadOnly.
var secretDirs = []string{"~/.ssh", "~/.aws", "~/.gnupg", "~/Library/Keychains"}

// Available lists the runners usable here, best first; "none" is always last
// (it is only ever used when named: "auto" never falls back to it).
func Available() []string {
	var rs []string
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err == nil {
			rs = append(rs, "sandbox-exec")
		}
	case "linux":
		if _, err := exec.LookPath("bwrap"); err == nil && bwrapStarts() == "" {
			rs = append(rs, "bwrap")
		}
	}
	return append(rs, "none")
}

var (
	bwrapOnce    sync.Once
	bwrapProblem string
)

// bwrapStarts: "" when bwrap can really make a sandbox here, else why not.
// Installed is not enough: Ubuntu 24.04 lets only programs with an AppArmor
// profile create user namespaces (kernel.apparmor_restrict_unprivileged_userns),
// and without one every bwrap run fails ("setting up uid map: Permission denied").
func bwrapStarts() string {
	bwrapOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--unshare-net", "true").CombinedOutput()
		if err != nil {
			bwrapProblem = strings.TrimSpace(string(out))
			if bwrapProblem == "" {
				bwrapProblem = err.Error()
			}
		}
	})
	return bwrapProblem
}

// ErrNoSandbox: "auto" found no sandbox runner. Box fails closed: running
// uncontained needs the runner "none", named explicitly.
var ErrNoSandbox = errors.New("box: no sandbox runner available (sandbox-exec / bwrap); name the runner \"none\" to run unsandboxed")

// Pick resolves a runner name: "auto"/"" → the best real sandbox or
// ErrNoSandbox; a named runner must be available.
func Pick(runner string) (string, error) {
	avail := Available()
	if runner == "" || runner == "auto" {
		if avail[0] == "none" {
			if runtime.GOOS == "linux" && bwrapProblem != "" {
				return "", fmt.Errorf("%w: bwrap is installed but cannot start (%s); on Ubuntu 24.04 give it an AppArmor profile with userns (see README)", ErrNoSandbox, bwrapProblem)
			}
			return "", ErrNoSandbox
		}
		return avail[0], nil
	}
	if !slices.Contains(avail, runner) {
		return "", fmt.Errorf("box: runner %q not available here (have %s)", runner, strings.Join(avail, ", "))
	}
	return runner, nil
}

// prepared: a command line rewritten into the runner, with its temp dir and proxy.
type prepared struct {
	runner string
	argv   []string
	env    []string
	px     *proxy
	tmp    string
}

func (pr *prepared) cleanup() {
	if pr.px != nil {
		pr.px.close()
	}
	if pr.tmp != "" {
		os.RemoveAll(pr.tmp)
	}
}

func prepare(runner string, p Policy, dir string, argv, env []string) (*prepared, error) {
	if len(argv) == 0 {
		return nil, errors.New("box: empty command")
	}
	runner, err := Pick(runner)
	if err != nil {
		return nil, err
	}
	pr := &prepared{runner: runner}
	if pr.tmp, err = os.MkdirTemp("", "warden-box-"); err != nil {
		return nil, fmt.Errorf("box: temp dir: %w", err)
	}
	tmp := realPath(pr.tmp)
	if env == nil {
		env = os.Environ()
	}
	pr.env = append(slices.Clone(env), "TMPDIR="+tmp+"/")
	port := 0
	if p.Net != nil {
		if pr.px, err = startProxy(p.Net); err != nil {
			pr.cleanup()
			return nil, err
		}
		port = pr.px.port()
		pr.env = append(pr.env, proxyEnv(pr.px.addr())...)
	}
	switch runner {
	case "sandbox-exec":
		pr.argv = append([]string{"sandbox-exec", "-p", profile(p, tmp, port)}, argv...)
	case "bwrap":
		pr.argv = append(append([]string{"bwrap"}, bwrapArgs(p, tmp, dir)...), argv...)
	case "none":
		pr.argv = slices.Clone(argv)
	default:
		pr.cleanup()
		return nil, fmt.Errorf("box: unknown runner %q", runner)
	}
	return pr, nil
}

// Command rewrites argv/env to run contained under runner, for callers that
// start the process themselves (MCP servers). cleanup stops the egress proxy
// and removes the temp dir: call it when the process is gone.
func Command(runner string, p Policy, argv, env []string) (argv2, env2 []string, cleanup func(), err error) {
	pr, err := prepare(runner, p, "", argv, env)
	if err != nil {
		return nil, nil, func() {}, err
	}
	return pr.argv, pr.env, pr.cleanup, nil
}

// Run runs argv in dir under runner (auto|sandbox-exec|bwrap|none). env nil
// means the current environment. A non-zero exit is returned as an error
// wrapping *exec.ExitError, with the output still in Result. "auto" with no
// sandbox available is ErrNoSandbox (never a silent "none").
func Run(ctx context.Context, runner string, p Policy, dir string, argv []string, env []string) (Result, error) {
	pr, err := prepare(runner, p, dir, argv, env)
	if err != nil {
		return Result{}, err
	}
	defer pr.cleanup()
	res := Result{Runner: pr.runner}
	cmd := exec.CommandContext(ctx, pr.argv[0], pr.argv[1:]...)
	var out bytes.Buffer
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, pr.env, &out, &out
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	res.Output = out.String()
	if pr.px != nil {
		res.Refused = pr.px.refusals()
	}
	if err != nil {
		return res, fmt.Errorf("box: %s %s: %w", pr.runner, argv[0], err)
	}
	return res, nil
}

func proxyEnv(addr string) []string {
	u := "http://" + addr
	var env []string
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY"} {
		env = append(env, k+"="+u, strings.ToLower(k)+"="+u)
	}
	return append(env, "NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1")
}

// expand resolves ~ and makes p absolute.
func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p
}

// realPath resolves symlinks (/tmp → /private/tmp) in the longest existing
// prefix of an absolute path; the sandbox matches real paths.
func realPath(p string) string {
	p = expand(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// paths returns each path both as given (expanded) and resolved, deduped.
func paths(ps []string) []string {
	var out []string
	for _, p := range ps {
		for _, q := range []string{expand(p), realPath(p)} {
			if !slices.Contains(out, q) {
				out = append(out, q)
			}
		}
	}
	return out
}

func sbQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func subpaths(ps []string) string {
	var b strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&b, " (subpath %s)", sbQuote(p))
	}
	return b.String()
}

// profile builds a deny-by-default Seatbelt profile. Later rules win, so
// ReadOnly can re-open a path under a secrets dir.
func profile(p Policy, tmp string, proxyPort int) string {
	var b strings.Builder
	b.WriteString(`(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))
(allow sysctl-read)
(allow file-read*)
(allow file-write* (literal "/dev/null") (literal "/dev/zero") (literal "/dev/tty") (regex #"^/dev/fd/"))
(allow file-ioctl (literal "/dev/null") (literal "/dev/zero") (literal "/dev/tty") (regex #"^/dev/fd/"))
(allow mach-lookup
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.system.logger")
  (global-name "com.apple.system.notification_center")
  (global-name "com.apple.trustd")
  (global-name "com.apple.trustd.agent")
  (global-name "com.apple.SecurityServer")
  (global-name "com.apple.ocspd"))
(allow ipc-posix-shm-read-data (ipc-posix-name "apple.shm.notification_center"))
`)
	fmt.Fprintf(&b, "(deny file-read*%s)\n", subpaths(paths(secretDirs)))
	if len(p.ReadOnly) > 0 {
		fmt.Fprintf(&b, "(allow file-read*%s)\n", subpaths(paths(p.ReadOnly)))
	}
	if p.Keychain {
		b.WriteString(keychainRules())
	}
	fmt.Fprintf(&b, "(allow file-read* file-write*%s)\n", subpaths(paths(append([]string{tmp}, p.ReadWrite...))))
	if proxyPort > 0 {
		fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", proxyPort)
	}
	return b.String()
}

// keychainRules: securityd's services and the keychain files (read only),
// plus the per-user mds cache securityd's clients write.
func keychainRules() string {
	var b strings.Builder
	b.WriteString(`(allow mach-lookup (global-name "com.apple.securityd.xpc") (global-name "com.apple.system.opendirectoryd.membership"))
(allow user-preference-read)
`)
	fmt.Fprintf(&b, "(allow file-read*%s)\n", subpaths(paths([]string{"~/Library/Keychains"})))
	if out, err := exec.Command("getconf", "DARWIN_USER_CACHE_DIR").Output(); err == nil {
		if c := strings.TrimSpace(string(out)); c != "" {
			fmt.Fprintf(&b, "(allow file-read* file-write*%s)\n", subpaths(paths([]string{filepath.Join(c, "mds")})))
		}
	}
	return b.String()
}

// bwrapArgs: the root read-only, secrets hidden, ReadWrite and the temp dir
// writable. With hosts allowed the network is shared and only the proxy env
// steers egress (bwrap cannot pin it to one port).
func bwrapArgs(p Policy, tmp, dir string) []string {
	a := []string{"--die-with-parent", "--new-session", "--unshare-pid", "--unshare-ipc",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc"}
	if len(p.Net) == 0 {
		a = append(a, "--unshare-net")
	}
	for _, s := range paths(secretDirs) {
		if fi, err := os.Stat(s); err == nil && fi.IsDir() {
			a = append(a, "--tmpfs", s)
		}
	}
	for _, s := range paths(p.ReadOnly) {
		a = append(a, "--ro-bind-try", s, s)
	}
	for _, s := range paths(append([]string{tmp}, p.ReadWrite...)) {
		a = append(a, "--bind-try", s, s)
	}
	if dir != "" {
		a = append(a, "--chdir", realPath(dir))
	}
	return append(a, "--")
}
