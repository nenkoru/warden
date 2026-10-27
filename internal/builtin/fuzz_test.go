package builtin

import (
	"strings"
	"testing"
)

func FuzzParseFeed(f *testing.F) {
	f.Add([]byte(`<rss><channel><item><title> Go 1.22 </title><link>https://go.dev/x</link><guid>g1</guid><description>&lt;p&gt;hi&lt;/p&gt;</description></item></channel></rss>`))
	f.Add([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>e1</id><title>v1.2.3</title><link href="https://x/y"/><content type="html">&lt;b&gt;fix&lt;/b&gt;</content></entry></feed>`))
	f.Add([]byte(`<feed><entry><summary>s</summary></entry></feed>`))
	f.Add([]byte(`not xml`))
	f.Fuzz(func(t *testing.T, b []byte) {
		items, err := parseFeed(b)
		if err != nil {
			return
		}
		for _, it := range items {
			if it.Title != strings.TrimSpace(it.Title) || it.ID != strings.TrimSpace(it.ID) {
				t.Fatalf("untrimmed item %+v", it)
			}
		}
	})
}

func FuzzParseGoMod(f *testing.F) {
	f.Add([]byte("module x\n\ngo 1.22\n\nrequire (\n\tgolang.org/x/text v0.3.7 // indirect\n\tgithub.com/a/b v1.0.0\n)\n\nrequire c.d/e v2.0.0\n"))
	f.Add([]byte("require (\n)\nrequire x\nrequire ( // c\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, c := range parseGoMod(b) {
			if c.Name == "" || c.Version == "" || c.Ecosystem != "Go" {
				t.Fatalf("%+v", c)
			}
		}
	})
}
