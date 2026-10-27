package version

import (
	"strings"
	"testing"
)

func TestUserAgent(t *testing.T) {
	ua := UserAgent()
	if !strings.HasPrefix(ua, "warden/"+Version+" ") || !strings.HasSuffix(ua, "(+"+URL+")") {
		t.Fatal(ua)
	}
}
