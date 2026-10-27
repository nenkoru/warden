package builtin

import (
	"context"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"

	"github.com/nenkoru/warden/internal/domain"
)

var majorSuffix = regexp.MustCompile(`/v[0-9]+$`)

type atomEntry struct {
	Title   string `xml:"title"`
	Updated string `xml:"updated"`
	ID      string `xml:"id"`
	Links   []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
	} `xml:"link"`
	Content string `xml:"content"`
	Summary string `xml:"summary"`
}

func (e atomEntry) link() string {
	for _, l := range e.Links {
		if l.Rel == "" || l.Rel == "alternate" {
			return l.Href
		}
	}
	if len(e.Links) > 0 {
		return e.Links[0].Href
	}
	return ""
}

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

// Releases: what changed in the watched GitHub repositories since last
// (repo → tag; updated in place). The first look is a baseline. A new release
// of something in the inventory is medium, of anything else info.
func Releases(ctx context.Context, repos []string, last map[string]string, comps []domain.Component, env *Env) ([]domain.Finding, []error) {
	w := env.web()
	if last == nil {
		last = map[string]string{}
	}
	var out []domain.Finding
	var errs []error
	for _, repo := range repos {
		var b []byte
		if err := w.do(ctx, "GET", w.GitHub+"/"+repo+"/releases.atom", nil, &b); err != nil {
			errs = append(errs, fmt.Errorf("releases %s: %w", repo, err))
			continue
		}
		var f atomFeed
		if err := xml.Unmarshal(b, &f); err != nil {
			errs = append(errs, fmt.Errorf("releases %s: %w", repo, err))
			continue
		}
		if len(f.Entries) == 0 {
			continue
		}
		e := f.Entries[0]
		tag := e.ID[strings.LastIndex(e.ID, "/")+1:]
		prev, known := last[repo]
		last[repo] = tag
		if !known || prev == tag {
			continue
		}
		sev, why := "info", ""
		for _, c := range comps {
			if strings.ToLower(majorSuffix.ReplaceAllString(c.Name, "")) != "github.com/"+strings.ToLower(repo) {
				continue
			}
			sev, why = "medium", fmt.Sprintf("in use: %s %s", c.Name, c.Version)
			if !semverLess(c.Version, tag) {
				sev, why = "info", why+" (already there)"
			}
		}
		pv, _ := splitVer(prev)
		if tv, _ := splitVer(tag); tv[0] > pv[0] {
			why = strings.TrimSpace(why + "; a new major version")
		}
		fd := finding("releases", "release|"+repo+"@"+tag, "news", sev, repo+"@"+tag, fmt.Sprintf("%s released %s (was %s)", repo, tag, prev),
			strings.TrimSpace(why+"\n"+firstLines(stripTags(e.Content), 8)))
		if l := e.link(); l != "" {
			fd.Links = []string{l}
		}
		fd.Data = map[string]string{"repo": repo, "tag": tag, "previous": prev}
		out = append(out, fd)
	}
	return out, errs
}

// FeedItem: one RSS item or Atom entry.
type FeedItem struct{ ID, Title, Link, Text string }

// Feed fetches an RSS 2.0 or Atom feed.
func Feed(ctx context.Context, url string, env *Env) ([]FeedItem, error) {
	var b []byte
	if err := env.web().do(ctx, "GET", url, nil, &b); err != nil {
		return nil, err
	}
	items, err := parseFeed(b)
	if err != nil {
		return nil, fmt.Errorf("feed %s: %w", url, err)
	}
	return items, nil
}

func parseFeed(b []byte) ([]FeedItem, error) {
	var rss struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			GUID        string `xml:"guid"`
			Description string `xml:"description"`
		} `xml:"channel>item"`
	}
	if xml.Unmarshal(b, &rss) == nil && len(rss.Items) > 0 {
		var out []FeedItem
		for _, it := range rss.Items {
			id := strings.TrimSpace(it.GUID)
			if id == "" {
				id = strings.TrimSpace(it.Link)
			}
			out = append(out, FeedItem{ID: id, Title: strings.TrimSpace(it.Title), Link: strings.TrimSpace(it.Link), Text: firstLines(stripTags(it.Description), 6)})
		}
		return out, nil
	}
	var atom atomFeed
	if err := xml.Unmarshal(b, &atom); err != nil {
		return nil, err
	}
	var out []FeedItem
	for _, e := range atom.Entries {
		body := e.Content
		if strings.TrimSpace(body) == "" {
			body = e.Summary
		}
		id := strings.TrimSpace(e.ID)
		if id == "" {
			id = e.link()
		}
		out = append(out, FeedItem{ID: id, Title: strings.TrimSpace(e.Title), Link: e.link(), Text: firstLines(stripTags(body), 6)})
	}
	return out, nil
}
