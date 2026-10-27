package core

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Durability between ticks belongs to the OS: launchd (macOS) or a systemd
// timer (Linux) starts `warden tick` every interval and after a reboot.
// Listen mode keeps one sleeping process alive instead. The units are
// written into the state folder; loading them is the owner's step.

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

func label(cfg *Config) string {
	return "dev.warden." + strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(cfg.Name), "-"), "-")
}

// Units writes the launchd plist and the systemd service (+timer); returns the paths.
func Units(cfg *Config, bin string, listen bool) ([]string, error) {
	bin, _ = filepath.Abs(bin)
	sec := int(cfg.Every().Seconds())
	l := label(cfg)
	logf := filepath.Join(cfg.State, "warden.log")
	mode, when := "tick", fmt.Sprintf("<key>StartInterval</key><integer>%d</integer>", sec)
	svc := "Type=oneshot"
	if listen {
		mode, when, svc = "listen", "<key>KeepAlive</key><true/>", "Type=simple\nRestart=always\nRestartSec=30"
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>%s</string><string>--config</string><string>%s</string></array>
  %s
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
  <key>Nice</key><integer>10</integer>
  <key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, l, bin, mode, cfg.Path, when, xmlEscape(unitPath()), logf, logf)
	service := fmt.Sprintf("[Unit]\nDescription=Warden %s\n\n[Service]\n%s\nEnvironment=PATH=%s\nExecStart=%s %s --config %s\nNice=10\nIOSchedulingClass=idle\n", cfg.Name, svc, unitPath(), bin, mode, cfg.Path)
	timer := fmt.Sprintf("[Unit]\nDescription=Warden %s every %s\n\n[Timer]\nOnBootSec=5min\nOnUnitActiveSec=%ds\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n", cfg.Name, cfg.Wake.Every, sec)
	dir := filepath.Join(cfg.State, "units")
	out := []string{filepath.Join(dir, l+".plist"), filepath.Join(dir, l+".service"), filepath.Join(dir, l+".timer")}
	bodies := []string{plist, service, timer}
	if listen {
		bodies[1] += "\n[Install]\nWantedBy=default.target\n"
		out, bodies = out[:2], bodies[:2]
	}
	for i, body := range bodies {
		if err := writeFile(out[i], []byte(body)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// unitPath: the installer's PATH (launchd and systemd start services with a
// minimal one, where go, npm, uv, kubectl … aren't found).
func unitPath() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	return "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
