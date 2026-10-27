package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const events = `{"items":[
 {"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container api","involvedObject":{"kind":"Pod","name":"api-7d9"}},
 {"type":"Normal","reason":"Pulled","message":"ok","involvedObject":{"kind":"Pod","name":"web-1"}},
 {"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container api","involvedObject":{"kind":"Pod","name":"api-7d9"}},
 {"type":"Warning","reason":"FailedMount","message":"secret \"db\" not found","involvedObject":{"kind":"Pod","name":"db-0"}}]}`

func TestMapFindingsFromKubectlJSON(t *testing.T) {
	m := &Map{Items: "items[]", Where: map[string]string{"type": "Warning"},
		Key: "{involvedObject.kind}/{involvedObject.name}/{reason}", Title: "{reason}: {involvedObject.kind}/{involvedObject.name} in {namespace}",
		Detail: "{message}", Subject: "{involvedObject.kind}/{involvedObject.name}"}
	fs, err := m.Findings([]byte(events), Step{Name: "warnings", Fix: "propose-manifest"}, map[string]string{"namespace": "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[0].Count != 2 || fs[0].Title != "BackOff: Pod/api-7d9 in shop" || fs[1].Detail != `secret "db" not found` {
		t.Fatalf("%+v", fs)
	}
	if fs[0].Fix == nil || fs[0].Fix.Act != "propose-manifest" || fs[0].Fix.Target != "Pod/api-7d9" || fs[0].ID == fs[1].ID {
		t.Fatalf("%+v", fs[0].Fix)
	}
	again, _ := m.Findings([]byte(events), Step{Name: "warnings"}, map[string]string{"namespace": "shop"})
	if again[0].ID != fs[0].ID {
		t.Fatal("ids must be stable across ticks")
	}
}

func TestMapComponentsFlattenAndParse(t *testing.T) {
	pods := `{"items":[{"spec":{"containers":[{"image":"nginx:1.25.3"},{"image":"ghcr.io/org/app:v2.1.0@sha256:abc"}]}},
	 {"spec":{"containers":[{"image":"nginx:1.25.3"},{"image":"localhost:5000/tool"}]}}]}`
	cs, err := (&Map{Items: "items[].spec.containers[]", Component: "{image}"}).Components([]byte(pods), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, c := range cs {
		got = append(got, c.Name+"="+c.Version)
	}
	if strings.Join(got, ",") != "ghcr.io/org/app=v2.1.0,localhost:5000/tool=,nginx=1.25.3" {
		t.Fatal(got)
	}
	for in, want := range map[string]string{"golang.org/x/text@v0.3.7": "golang.org/x/text v0.3.7", "requests==2.31.0": "requests 2.31.0", "php 8.3.12": "php 8.3.12"} {
		c := ParseComponent(in)
		if c.Name+" "+c.Version != want {
			t.Errorf("%s → %+v", in, c)
		}
	}
}

func TestMapLinesAndNative(t *testing.T) {
	logs := "12:00 GET / 200\n12:01 panic: index out of range [3]\n12:02 panic: index out of range [3]\n"
	fs, err := (&Map{Lines: `panic: (?P<what>.*)`, Key: "{what}", Title: "panic: {what}", Severity: "high"}).Findings([]byte(logs), Step{Name: "log"}, nil)
	if err != nil || len(fs) != 1 || fs[0].Count != 2 || fs[0].Severity != "high" {
		t.Fatal(fs, err)
	}
	native := `{"key":"disk","kind":"low_disk","severity":"high","subject":"/","title":"12 GB free","fix":{"act":"tool-clean","what":"clean","target":"go-build"}}`
	var nilMap *Map
	fs, err = nilMap.Findings([]byte(native+"\n"), Step{Name: "disk"}, nil)
	if err != nil || len(fs) != 1 || fs[0].Fix.Target != "go-build" || fs[0].ID == "" {
		t.Fatal(fs, err)
	}
	if _, err := nilMap.Findings([]byte("plain text\n"), Step{Name: "x"}, nil); err == nil {
		t.Fatal("unmapped text must be an error that says to add a map")
	}
}

func TestLoadValidates(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) { _ = os.WriteFile(filepath.Join(dir, "domain.toml"), []byte(s), 0o644) }
	write(`name = "ns"
params = { namespace = "default" }
[mcp.kubernetes]
registry = "mcp"
name = "io.github.containers/kubernetes-mcp-server"
[[observe]]
name = "warnings"
fix = "propose"
run = { mcp = "kubernetes", tool = "events_list", args = { namespace = "{namespace}" } }
[[act]]
name = "propose"
run = { builtin = "propose" }
floors = { reversible = true }
[[act]]
name = "apply"
run = { cmd = ["kubectl", "apply", "-n", "{namespace}", "-f", "{file}"] }
floors = { grant = "k8s.apply", always_ask = true }
`)
	d, err := Load(dir, map[string]string{"namespace": "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Params["namespace"] != "shop" || !d.Act[1].Floors.AlwaysAsk || d.Act[1].Floors.Grant != "k8s.apply" {
		t.Fatalf("%+v", d)
	}
	if a := Expand(d.Act[1].Run.Cmd, map[string]string{"namespace": "shop", "file": "/x.yaml"}); strings.Join(a, " ") != "kubectl apply -n shop -f /x.yaml" {
		t.Fatal(a)
	}
	if args := ExpandArgs(d.Observe[0].Run.Args, d.Params); args["namespace"] != "shop" {
		t.Fatal(args)
	}
	write("name = \"x\"\n[[observe]]\nname = \"a\"\nrun = { cmd = [\"true\"], builtin = \"disk\" }\n")
	if _, err := Load(dir, nil); err == nil {
		t.Fatal("two runs in one step must be refused")
	}
	write("name = \"x\"\nfloor = \"off\"\n")
	if _, err := Load(dir, nil); err == nil {
		t.Fatal("unknown fields must be refused")
	}
}

func TestShippedPacksLoad(t *testing.T) {
	for _, p := range []string{"go-project", "macos", "k8s-namespace", "linux-server"} {
		if _, err := Load(filepath.Join("..", "..", "packs", p), nil); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestLinuxServerMaps(t *testing.T) {
	d, err := Load(filepath.Join("..", "..", "packs", "linux-server"), nil)
	if err != nil {
		t.Fatal(err)
	}
	units := "caddy.service loaded failed failed Caddy web server\nbackup.timer loaded failed failed Nightly backup\n"
	fs, err := d.Observe[1].Map.Findings([]byte(units), d.Observe[1], d.Params)
	if err != nil || len(fs) != 2 || fs[0].Title != "failed unit: caddy.service" || fs[0].Detail != "Caddy web server" || fs[0].Fix.Act != "propose" {
		t.Fatal(fs, err)
	}
	cs, err := d.Inventory[0].Map.Components([]byte("openssl 3.0.13-0ubuntu3\nphp8.3 8.3.6-0maysync1\n"), d.Params)
	if err != nil || len(cs) != 2 || cs[0].Name != "openssl" || cs[0].Version != "3.0.13-0ubuntu3" || cs[0].Ecosystem != "Debian" {
		t.Fatal(cs, err)
	}
}

func TestK8sNamespaceForeachAndMaps(t *testing.T) {
	d, err := Load(filepath.Join("..", "..", "packs", "k8s-namespace"), map[string]string{"namespace": "shop", "namespaces": "shop, shop-db"})
	if err != nil {
		t.Fatal(err)
	}
	var jobs, charts Step
	for _, s := range d.Observe {
		if s.Name == "failed-jobs" {
			jobs = s
		}
	}
	for _, s := range d.Inventory {
		if s.Name == "helm-charts" {
			charts = s
		}
	}
	sets, bound := jobs.Each(d.Params)
	if len(sets) != 2 || sets[1]["namespace"] != "shop-db" || bound[0]["namespace"] != "shop" || sets[0]["context"] != "" {
		t.Fatal(sets, bound)
	}
	if a := Expand(jobs.Run.Cmd, sets[1]); a[5] != "shop-db" || !strings.Contains(a[7], `{"\t"}`) {
		t.Fatal(a)
	}
	// kubectl's jsonpath output: two failed runs of one CronJob, one that completed, one bare Job
	out := "refresh-29885592\tCronJob/refresh\tBackoffLimitExceeded\tJob has reached the specified backoff limit\n" +
		"refresh-29885596\tCronJob/refresh\tBackoffLimitExceeded\tJob has reached the specified backoff limit\n" +
		"backup-29885000\tCronJob/backup\t\t\n" +
		"probe\t/\tDeadlineExceeded\tJob was active longer than specified deadline\n"
	fs, err := jobs.Map.Findings([]byte(out), jobs, sets[1])
	if err != nil || len(fs) != 2 || fs[0].Count != 2 || fs[0].Key != "shop-db/CronJob/refresh/BackoffLimitExceeded" ||
		fs[0].Title != "Job failed (BackoffLimitExceeded): shop-db/refresh-29885592 of CronJob/refresh" || fs[1].Subject != "/" {
		t.Fatal(fs, err)
	}
	cs, err := charts.Map.Components([]byte("coder-2.16.1\n\nkube-prometheus-stack-62.3.0\ncoder-2.16.1\nprefect-server-2026.3.19205444\n"), sets[0])
	if err != nil || len(cs) != 3 || cs[0].Name != "coder" || cs[0].Version != "2.16.1" || cs[0].Ecosystem != "Helm" ||
		cs[1].Name != "kube-prometheus-stack" || cs[2].Version != "2026.3.19205444" {
		t.Fatal(cs, err)
	}
	// no namespaces: one run with {namespace} as it is
	d1, _ := Load(filepath.Join("..", "..", "packs", "k8s-namespace"), map[string]string{"namespace": "shop"})
	if sets, bound := d1.Observe[0].Each(d1.Params); len(sets) != 1 || sets[0]["namespace"] != "shop" || bound[0] != nil {
		t.Fatal(sets, bound)
	}
	write := filepath.Join(t.TempDir(), "domain.toml")
	os.WriteFile(write, []byte("name = \"x\"\n[[act]]\nname = \"a\"\nforeach = { n = \"{ns}\" }\nrun = { cmd = [\"true\"] }\n"), 0o644)
	if _, err := Load(filepath.Dir(write), nil); err == nil {
		t.Fatal("foreach on an act must be refused")
	}
}
