package builtin

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/domain"
)

// ecosystems OSV knows (inventory names match OSV's).
var osvEcosystems = map[string]bool{"Go": true, "PyPI": true, "npm": true, "crates.io": true, "Maven": true, "NuGet": true,
	"RubyGems": true, "Packagist": true, "Pub": true, "Hex": true}

type osvVuln struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Aliases  []string `json:"aliases"`
	Affected []struct {
		Package struct{ Ecosystem, Name string } `json:"package"`
		Ranges  []struct {
			Type   string              `json:"type"`
			Events []map[string]string `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
	DatabaseSpecific struct {
		Severity string `json:"severity"` // GHSA: LOW | MODERATE | HIGH | CRITICAL
	} `json:"database_specific"`
}

// fixedAfter: the smallest fixed version above the current one, for this package.
func (v *osvVuln) fixedAfter(c domain.Component) string {
	best := ""
	for _, a := range v.Affected {
		if a.Package.Name != c.Name || !strings.EqualFold(a.Package.Ecosystem, c.Ecosystem) {
			continue
		}
		for _, r := range a.Ranges {
			for _, e := range r.Events {
				if fx := e["fixed"]; fx != "" && semverLess(c.Version, fx) && (best == "" || semverLess(fx, best)) {
					best = fx
				}
			}
		}
	}
	return best
}

func osvSeverity(s string) string {
	switch strings.ToUpper(s) {
	case "CRITICAL":
		return "critical"
	case "HIGH":
		return "high"
	case "MODERATE", "MEDIUM":
		return "medium"
	case "LOW":
		return "low"
	}
	return ""
}

// groupAliases: advisories that alias each other (in either direction) are one.
func groupAliases(vs []*osvVuln) [][]*osvVuln {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		parent[x] = x
		return x
	}
	for _, v := range vs {
		for _, a := range v.Aliases {
			if ra, rv := find(a), find(v.ID); ra != rv {
				parent[ra] = rv
			}
		}
	}
	idx := map[string]int{}
	var out [][]*osvVuln
	for _, v := range vs {
		r := find(v.ID)
		i, ok := idx[r]
		if !ok {
			i = len(out)
			idx[r] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], v)
	}
	return out
}

// Advisories: one finding per vulnerable component (its advisories, aliases
// merged), with the fix: the smallest version that fixes them all.
func Advisories(ctx context.Context, comps []domain.Component, fix map[string]string, env *Env) ([]domain.Finding, error) {
	w := env.web()
	var cs []domain.Component
	for _, c := range comps {
		if osvEcosystems[c.Ecosystem] && c.Name != "" && c.Version != "" {
			cs = append(cs, c)
		}
	}
	type pkg struct {
		Ecosystem string `json:"ecosystem"`
		Name      string `json:"name"`
	}
	type query struct {
		Package pkg    `json:"package"`
		Version string `json:"version"`
	}
	ids := make([][]string, len(cs))
	for start := 0; start < len(cs); start += 1000 {
		end := min(start+1000, len(cs))
		var qs []query
		for _, c := range cs[start:end] {
			qs = append(qs, query{pkg{c.Ecosystem, c.Name}, strings.TrimPrefix(c.Version, "v")})
		}
		var res struct {
			Results []struct {
				Vulns []struct{ ID string } `json:"vulns"`
			} `json:"results"`
		}
		if err := w.do(ctx, "POST", w.OSV+"/v1/querybatch", map[string]any{"queries": qs}, &res); err != nil {
			return nil, fmt.Errorf("osv querybatch: %w", err)
		}
		for i, r := range res.Results {
			if start+i < end {
				for _, v := range r.Vulns {
					ids[start+i] = append(ids[start+i], v.ID)
				}
			}
		}
	}
	cache := map[string]*osvVuln{}
	get := func(id string) (*osvVuln, error) {
		if v := cache[id]; v != nil {
			return v, nil
		}
		v := &osvVuln{}
		if err := w.do(ctx, "GET", w.OSV+"/v1/vulns/"+url.PathEscape(id), nil, v); err != nil {
			return nil, fmt.Errorf("osv %s: %w", id, err)
		}
		if v.ID == "" {
			v.ID = id
		}
		cache[id] = v
		return v, nil
	}
	var out []domain.Finding
	for i, c := range cs {
		if len(ids[i]) == 0 {
			continue
		}
		// every listed id is fetched (an alias still lends its grade and fix); only distinct advisories are counted
		var vs []*osvVuln
		for _, id := range ids[i] {
			v, err := get(id)
			if err != nil {
				return nil, err
			}
			vs = append(vs, v)
		}
		var names, titles, links, unfixed []string
		sev, to := "", ""
		for _, g := range groupAliases(vs) {
			id, summary, gfix := g[0].ID, "", ""
			for _, v := range g {
				if summary == "" {
					summary = v.Summary
				}
				if s := osvSeverity(v.DatabaseSpecific.Severity); s != "" && (sev == "" || domain.Severities[s] > domain.Severities[sev]) {
					sev = s
				}
				if fx := v.fixedAfter(c); fx != "" && (gfix == "" || semverLess(fx, gfix)) {
					gfix = fx
				}
			}
			names = append(names, id)
			if summary != "" {
				titles = append(titles, id+": "+summary)
			} else {
				titles = append(titles, id)
			}
			links = append(links, "https://osv.dev/vulnerability/"+id)
			if gfix == "" {
				unfixed = append(unfixed, id)
			} else if to == "" || semverLess(to, gfix) {
				to = gfix
			}
		}
		if sev == "" {
			sev = "medium" // the Go vulnerability database does not grade: assume medium
		}
		if c.Ecosystem == "Go" {
			to = vprefix(to)
		}
		plural := "ies"
		if len(names) == 1 {
			plural = "y"
		}
		detail := strings.Join(titles, "\n")
		if len(unfixed) > 0 {
			detail += "\nno fixed version yet: " + strings.Join(unfixed, ", ")
		}
		key := "vuln|" + c.Ecosystem + "|" + c.Name + "@" + c.Version
		f := finding("osv", key, "vuln", sev, c.Name+"@"+c.Version, fmt.Sprintf("%s %s: %d known vulnerabilit%s", c.Name, c.Version, len(names), plural), detail)
		f.Links = links
		params := map[string]string{"ecosystem": c.Ecosystem, "module": c.Name, "from": c.Version, "to": to, "advisories": strings.Join(names, ",")}
		if act := fix[c.Ecosystem]; act != "" && to != "" {
			params["published"], params["fresh"] = env.freshness(ctx, c.Ecosystem, c.Name, to)
			what := fmt.Sprintf("bump %s %s → %s in a branch, run the checks", c.Name, c.Version, to)
			if params["fresh"] == "true" {
				what += " (fix published " + params["published"] + ": younger than the minimum age)"
			}
			f.Fix = &domain.Fix{Act: act, What: what, Target: c.Name, Params: params}
		}
		f.Data = maps.Clone(params)
		out = append(out, f)
	}
	return out, nil
}

// freshness: when the fix version was published and whether it is younger
// than min_age_days (a brand new release can be broken or malicious).
func (e *Env) freshness(ctx context.Context, eco, module, version string) (published, fresh string) {
	days, err := strconv.Atoi(param(nil, e, "min_age_days", "14"))
	if err != nil {
		days = 14
	}
	w := e.web()
	var at time.Time
	switch eco {
	case "Go":
		var info struct{ Time time.Time }
		if w.do(ctx, "GET", w.Proxy+"/"+escapeModule(module)+"/@v/"+escapeModule(version)+".info", nil, &info) == nil {
			at = info.Time
		}
	case "npm": // the packument's time map: version → published
		var doc struct {
			Time map[string]time.Time `json:"time"`
		}
		if w.NPM != "" && w.do(ctx, "GET", w.NPM+"/"+strings.Replace(module, "/", "%2F", 1), nil, &doc) == nil {
			at = doc.Time[version]
		}
	case "PyPI": // the release's files: the earliest upload
		var rel struct {
			URLs []struct {
				Upload time.Time `json:"upload_time_iso_8601"`
			} `json:"urls"`
		}
		if w.PyPI != "" && w.do(ctx, "GET", w.PyPI+"/pypi/"+module+"/"+version+"/json", nil, &rel) == nil {
			for _, u := range rel.URLs {
				if at.IsZero() || u.Upload.Before(at) {
					at = u.Upload
				}
			}
		}
	}
	if at.IsZero() {
		return "unknown", "true"
	}
	return at.UTC().Format("2006-01-02"), strconv.FormatBool(e.now().Sub(at) < time.Duration(days)*24*time.Hour)
}

// escapeModule: the Go proxy's case encoding (Uppercase → !lowercase).
func escapeModule(m string) string {
	var b strings.Builder
	for _, r := range m {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
