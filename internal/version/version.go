// Package version: Warden's version and the User-Agent it sends.
package version

// Version is set at build time:
//
//	go build -ldflags "-X github.com/nenkoru/warden/internal/version.Version=v0.1.0"
var Version = "dev"

// URL is where Warden lives; it goes into the User-Agent so that site
// operators can tell what is fetching their feeds.
const URL = "https://github.com/nenkoru/warden"

// UserAgent: one descriptive User-Agent for every outgoing HTTP request
// (some sites, e.g. CISA's feeds, refuse requests without one).
func UserAgent() string { return "warden/" + Version + " (+" + URL + ")" }
