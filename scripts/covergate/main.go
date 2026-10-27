// Command covergate: CI's coverage gate. It reads the total from
// `go tool cover -func` output and either records it (-out: {"total","sha"},
// the artifact a push to main uploads) or compares it with main's record
// (-base): a drop of more than -max-drop percentage points fails; no record
// (a first run, an expired artifact) passes with a notice.
//
//	go tool cover -func=cover.out > cover.txt
//	go run ./scripts/covergate -func cover.txt -sha "$GITHUB_SHA" -out coverage.json
//	go run ./scripts/covergate -func cover.txt -base base/coverage.json
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Record: what a main run leaves for the pull requests after it.
type Record struct {
	Total float64 `json:"total"` // percent of statements
	SHA   string  `json:"sha"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }

func run(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("covergate", flag.ContinueOnError)
	fs.SetOutput(out)
	funcFile := fs.String("func", "", "`go tool cover -func` output")
	sha := fs.String("sha", "", "the commit (recorded with -out)")
	outFile := fs.String("out", "", "write the record here")
	baseFile := fs.String("base", "", "main's record to compare with (missing = no baseline)")
	maxDrop := fs.Float64("max-drop", 0.1, "the largest drop allowed, in percentage points")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *funcFile == "" || (*outFile == "" && *baseFile == "") {
		fmt.Fprintln(out, "covergate: want -func and -out and/or -base")
		return 2
	}
	f, err := os.Open(*funcFile)
	if err != nil {
		fmt.Fprintf(out, "::error::covergate: %v\n", err)
		return 1
	}
	total, err := parseTotal(f)
	f.Close()
	if err != nil {
		fmt.Fprintf(out, "::error::covergate: %s: %v\n", *funcFile, err)
		return 1
	}
	fmt.Fprintf(out, "coverage: %.1f%%\n", total)
	if *outFile != "" {
		b, _ := json.Marshal(Record{Total: total, SHA: *sha})
		if err := os.WriteFile(*outFile, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintf(out, "::error::covergate: %v\n", err)
			return 1
		}
	}
	if *baseFile == "" {
		return 0
	}
	base, err := readRecord(*baseFile)
	if err != nil {
		fmt.Fprintf(out, "::error::covergate: %s: %v\n", *baseFile, err)
		return 1
	}
	ok, msg := compare(base, total, *maxDrop)
	if !ok {
		fmt.Fprintf(out, "::error::%s\n", msg)
		return 1
	}
	if base == nil {
		fmt.Fprintf(out, "::notice::%s\n", msg)
	} else {
		fmt.Fprintln(out, msg)
	}
	return 0
}

// parseTotal: the percentage on the "total:" line.
func parseTotal(r io.Reader) (float64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] != "total:" {
			continue
		}
		p, ok := strings.CutSuffix(f[len(f)-1], "%")
		if !ok {
			return 0, fmt.Errorf("total line %q: no percentage", sc.Text())
		}
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 || v > 100 {
			return 0, fmt.Errorf("total line %q: bad percentage", sc.Text())
		}
		return v, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("no total line")
}

// readRecord: nil when the file does not exist.
func readRecord(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Total < 0 || r.Total > 100 {
		return nil, fmt.Errorf("total %v out of range", r.Total)
	}
	return &r, nil
}

// compare: whether cur may replace base (a drop of at most maxDrop points).
func compare(base *Record, cur, maxDrop float64) (bool, string) {
	if base == nil {
		return true, fmt.Sprintf("coverage %.1f%%: no baseline from main yet, not compared", cur)
	}
	drop := base.Total - cur
	at := ""
	if base.SHA != "" {
		at = " at " + short(base.SHA)
	}
	if drop > maxDrop+1e-9 {
		return false, fmt.Sprintf("coverage dropped %.2f points: %.1f%% on main%s, %.1f%% here (at most %.1f allowed)", drop, base.Total, at, cur, maxDrop)
	}
	return true, fmt.Sprintf("coverage %.1f%% (main%s: %.1f%%, %+.2f points)", cur, at, base.Total, -drop)
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
