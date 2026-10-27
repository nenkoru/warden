package builtin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nenkoru/warden/internal/domain"
)

// The machine observers only read. Cleaning is done by the tools' own
// commands (tool-clean), never by deleting paths.

// Cleaner: a regenerable cache and the tool's own way to clean it.
type Cleaner struct {
	ID, Title, Risk, Note string
	Path                  func() string
	Argv                  []string // nil: report only
}

func home(p string) string { h, _ := os.UserHomeDir(); return filepath.Join(h, p) }

// cmdOut: a command's trimmed stdout, "" on failure (a var for tests).
var cmdOut = func(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func goEnv(k string) func() string {
	return func() string { return cmdOut(context.Background(), "go", "env", k) }
}

// Cleaners: the built-in list (the allowlist: nothing else is ever cleaned).
func Cleaners() []Cleaner {
	return []Cleaner{
		{ID: "go-build", Title: "Go build cache", Path: goEnv("GOCACHE"), Argv: []string{"go", "clean", "-cache"}, Risk: "low", Note: "rebuilt on the next build"},
		{ID: "go-mod", Title: "Go module cache", Path: goEnv("GOMODCACHE"), Argv: []string{"go", "clean", "-modcache"}, Risk: "medium", Note: "re-downloaded on the next build; offline builds break until then"},
		{ID: "npm", Title: "npm cache", Path: func() string { return home(".npm/_cacache") }, Argv: []string{"npm", "cache", "clean", "--force"}, Risk: "low"},
		{ID: "pnpm", Title: "pnpm store (unreferenced)", Path: func() string { return cmdOut(context.Background(), "pnpm", "store", "path") }, Argv: []string{"pnpm", "store", "prune"}, Risk: "low", Note: "only packages no project references"},
		{ID: "yarn", Title: "Yarn cache", Path: func() string { return home("Library/Caches/Yarn") }, Argv: []string{"yarn", "cache", "clean"}, Risk: "low"},
		{ID: "uv", Title: "uv cache (unused)", Path: func() string { return home(".cache/uv") }, Argv: []string{"uv", "cache", "prune"}, Risk: "low", Note: "only entries nothing uses"},
		{ID: "pip", Title: "pip cache", Path: func() string { return home("Library/Caches/pip") }, Argv: []string{"pip3", "cache", "purge"}, Risk: "low"},
		{ID: "simulators", Title: "iOS simulators (unavailable runtimes)", Path: func() string { return home("Library/Developer/CoreSimulator/Devices") }, Argv: []string{"xcrun", "simctl", "delete", "unavailable"}, Risk: "low", Note: "only simulators whose runtime is gone; the size is of all simulators"},
		{ID: "homebrew", Title: "Homebrew downloads", Path: func() string { return home("Library/Caches/Homebrew") }, Risk: "low", Note: "`brew cleanup --prune=all` is yours to run"},
		{ID: "xcode-derived", Title: "Xcode DerivedData", Path: func() string { return home("Library/Developer/Xcode/DerivedData") }, Risk: "low", Note: "Xcode rebuilds it; clear it from Xcode (Settings → Locations) when no build is open"},
		{ID: "huggingface", Title: "Hugging Face models", Path: func() string { return home(".cache/huggingface") }, Risk: "medium", Note: "downloaded models: expensive to fetch again — yours to prune"},
	}
}

// test hooks
var (
	cleaners   = Cleaners
	dataVolume = "/System/Volumes/Data"
	diskFree   = func(path string) (free, total int64, err error) {
		var s syscall.Statfs_t
		if err = syscall.Statfs(path, &s); err != nil {
			return
		}
		return int64(s.Bavail) * int64(s.Bsize), int64(s.Blocks) * int64(s.Bsize), nil
	}
	du = duSK
)

// duSK: bytes under a path (du -sk), bounded in time; -1 if unknown.
func duSK(ctx context.Context, p string) int64 {
	if p == "" {
		return -1
	}
	if _, err := os.Stat(p); err != nil {
		return -1
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "du", "-sk", p).Output() // exits 1 on unreadable entries: the sum is still printed
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return -1
	}
	kb, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return -1
	}
	return kb * 1024
}

// Observe runs one machine observer.
func Observe(ctx context.Context, name string, with map[string]any, env *Env) ([]domain.Finding, error) {
	switch name {
	case "disk":
		return observeDisk(ctx, with, env)
	case "caches":
		return observeCaches(ctx), nil
	case "library-caches":
		return libraryCaches(ctx), nil
	case "trash":
		if n := du(ctx, home(".Trash")); n > 1<<30 {
			f := finding(name, "trash", "trash", "low", "~/.Trash", "Trash holds "+gb(n), "emptying the trash is yours (Warden never deletes your files)")
			f.Data = map[string]string{"bytes": strconv.FormatInt(n, 10)}
			return []domain.Finding{f}, nil
		}
		return nil, nil
	case "snapshots":
		if n := strings.Count(cmdOut(ctx, "tmutil", "listlocalsnapshots", "/"), "com.apple.TimeMachine"); n > 0 {
			return []domain.Finding{finding(name, "snapshots", "snapshots", "info", "/", fmt.Sprintf("%d local Time Machine snapshot(s)", n),
				"macOS frees these by itself when space runs low")}, nil
		}
		return nil, nil
	case "health":
		return health(ctx), nil
	case "github-issues", "github-prs", "github-main", "github-pause":
		return observeGitHub(ctx, name, env)
	}
	return nil, fmt.Errorf("builtin observe %q: unknown", name)
}

type cacheSize struct {
	c     Cleaner
	path  string
	bytes int64
}

// measureCaches: every cleaner's size, in parallel, biggest first.
func measureCaches(ctx context.Context) []cacheSize {
	cl := cleaners()
	out := make([]cacheSize, len(cl))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range cl {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p := ""
			if c.Path != nil {
				p = c.Path()
			}
			out[i] = cacheSize{c, p, du(ctx, p)}
		}()
	}
	wg.Wait()
	sort.SliceStable(out, func(a, b int) bool { return out[a].bytes > out[b].bytes })
	return out
}

func observeCaches(ctx context.Context) []domain.Finding {
	var out []domain.Finding
	for _, s := range measureCaches(ctx) {
		if s.bytes < 512<<20 { // under half a gigabyte: not worth a line
			continue
		}
		f := finding("caches", "cache|"+s.c.ID, "cache", "low", s.path, fmt.Sprintf("%s: %s", s.c.Title, gb(s.bytes)), s.c.Note)
		f.Data = map[string]string{"cleaner": s.c.ID, "bytes": strconv.FormatInt(s.bytes, 10), "risk": s.c.Risk}
		if s.c.Argv != nil {
			argv := strings.Join(s.c.Argv, " ")
			f.Fix = &domain.Fix{Act: "tool-clean", Target: s.c.ID, What: fmt.Sprintf("run `%s` (frees up to %s)", argv, gb(s.bytes)),
				Params: map[string]string{"cleaner": s.c.ID, "argv": argv, "bytes": strconv.FormatInt(s.bytes, 10)}}
		}
		out = append(out, f)
	}
	return out
}

func observeDisk(ctx context.Context, with map[string]any, env *Env) ([]domain.Finding, error) {
	minGB, err := strconv.ParseFloat(param(with, env, "min_free_gb", "25"), 64)
	if err != nil {
		return nil, fmt.Errorf("disk: min_free_gb: %w", err)
	}
	vol := dataVolume
	if _, err := os.Stat(vol); err != nil {
		vol = "/"
	}
	free, total, err := diskFree(vol)
	if err != nil {
		return nil, fmt.Errorf("disk %s: %w", vol, err)
	}
	limit := minGB * (1 << 30)
	if float64(free) >= limit {
		return nil, nil
	}
	sev := "medium"
	if float64(free) < limit/2 {
		sev = "high"
	}
	d := fmt.Sprintf("%s free of %s (%.0f%% used); threshold %.0f GB.", gb(free), gb(total), 100-100*float64(free)/float64(max(total, 1)), minGB)
	var reclaim int64
	var top []string
	for _, s := range measureCaches(ctx) {
		if s.c.Argv != nil && s.bytes >= 512<<20 {
			reclaim += s.bytes
			if len(top) < 5 {
				top = append(top, s.c.Title+" "+gb(s.bytes))
			}
		}
	}
	if reclaim > 0 {
		d += fmt.Sprintf(" Regenerable caches the tools can clean: %s. Biggest: %s.", gb(reclaim), strings.Join(top, ", "))
	}
	f := finding("disk", "low_disk|"+vol, "low_disk", sev, vol, "Low disk space: "+gb(free)+" free", d)
	f.Data = map[string]string{"free": strconv.FormatInt(free, 10), "total": strconv.FormatInt(total, 10), "reclaimable": strconv.FormatInt(reclaim, 10)}
	return []domain.Finding{f}, nil
}

// libraryCaches: ~/Library/Caches by app, report only (apps own them).
func libraryCaches(ctx context.Context) []domain.Finding {
	root := home("Library/Caches")
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type sz struct {
		name string
		n    int64
	}
	var list []sz
	var total int64
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if n := du(ctx, filepath.Join(root, e.Name())); n > 0 {
			list = append(list, sz{e.Name(), n})
			total += n
		}
	}
	if total < 2<<30 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	var top []string
	for i := 0; i < len(list) && i < 6; i++ {
		top = append(top, list[i].name+" "+gb(list[i].n))
	}
	f := finding("library-caches", "library-caches", "cache", "info", "~/Library/Caches", "~/Library/Caches: "+gb(total),
		"apps own these; the biggest: "+strings.Join(top, ", ")+". Report only.")
	f.Data = map[string]string{"bytes": strconv.FormatInt(total, 10)}
	return []domain.Finding{f}
}

var (
	reCycles  = regexp.MustCompile(`Cycle Count:\s*(\d+)`)
	reCond    = regexp.MustCompile(`Condition:\s*([A-Za-z ]+)`)
	reMaxCap  = regexp.MustCompile(`Maximum Capacity:\s*(\d+)%`)
	reCPULimt = regexp.MustCompile(`CPU_Speed_Limit\s*=\s*(\d+)`)
)

// health: battery condition, thermal throttling.
func health(ctx context.Context) []domain.Finding {
	var out []domain.Finding
	sp := cmdOut(ctx, "system_profiler", "SPPowerDataType")
	if m := reCond.FindStringSubmatch(sp); m != nil {
		cond := strings.TrimSpace(m[1])
		title := "Battery: " + cond
		if c := reCycles.FindStringSubmatch(sp); c != nil {
			title += ", " + c[1] + " cycles"
		}
		if c := reMaxCap.FindStringSubmatch(sp); c != nil {
			title += ", max capacity " + c[1] + "%"
		}
		sev := "info"
		if cond != "Normal" {
			sev = "medium"
		}
		out = append(out, finding("health", "battery|"+cond, "health", sev, "battery", title, ""))
	}
	if m := reCPULimt.FindStringSubmatch(cmdOut(ctx, "pmset", "-g", "therm")); m != nil {
		if n, _ := strconv.Atoi(m[1]); n < 100 {
			out = append(out, finding("health", "thermal|"+m[1], "health", "medium", "cpu", fmt.Sprintf("CPU is thermally limited to %d%%", n), ""))
		}
	}
	return out
}
