package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

// maxBinary: the largest warden binary accepted from an archive.
const maxBinary = 256 << 20

// extract: the warden binary from a release archive — the one regular file
// named warden at the top (or one folder down); a path with .. or an absolute
// one is refused, as is a second warden or one over maxBinary.
func extract(archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	tr := tar.NewReader(zr)
	var bin []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		name := h.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || slicesContains(strings.Split(name, "/"), "..") {
			return nil, fmt.Errorf("archive: unsafe path %q", name)
		}
		clean := path.Clean(name)
		if path.Base(clean) != "warden" || strings.Count(clean, "/") > 1 {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("archive: %s is not a regular file", name)
		}
		if bin != nil {
			return nil, errors.New("archive: two warden binaries")
		}
		if h.Size > maxBinary {
			return nil, fmt.Errorf("archive: %s is larger than %d bytes", name, maxBinary)
		}
		if bin, err = io.ReadAll(io.LimitReader(tr, maxBinary+1)); err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		if len(bin) > maxBinary {
			return nil, fmt.Errorf("archive: %s is larger than %d bytes", name, maxBinary)
		}
	}
	if len(bin) == 0 {
		return nil, errors.New("archive: no warden binary in it")
	}
	return bin, nil
}

func slicesContains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// install puts the new binary in place: <bin>.new (0755, preflighted:
// it must start and read this warden.toml) → the old one kept as
// <bin>.prev → <bin>.new renamed over <bin> (same folder: atomic).
func (u *Updater) install(ctx context.Context, tag string, bin []byte) error {
	target, err := u.binPath()
	if err != nil {
		return err
	}
	next, prev := target+".new", target+".prev"
	_ = os.Remove(next)
	if err := os.WriteFile(next, bin, 0o755); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	if err := os.Chmod(next, 0o755); err != nil { // the umask may have taken bits
		os.Remove(next)
		return fmt.Errorf("install: %w", err)
	}
	if err := u.preflight(ctx, next, tag); err != nil {
		os.Remove(next)
		return err
	}
	_ = os.Remove(prev)
	if err := os.Link(target, prev); err != nil { // keep the old one; a copy where links are not allowed
		if err := copyFile(target, prev); err != nil {
			os.Remove(next)
			return fmt.Errorf("install: keeping the previous binary: %w", err)
		}
	}
	if err := os.Rename(next, target); err != nil {
		os.Remove(next)
		return fmt.Errorf("install: %w", err)
	}
	return nil
}

// restore puts <bin>.prev back over <bin>.
func (u *Updater) restore() error {
	target, err := u.binPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(target + ".prev"); err != nil {
		return fmt.Errorf("no previous binary to restore: %w", err)
	}
	return os.Rename(target+".prev", target)
}

// binPath: the binary to replace — a regular file, never through a symlink
// (that would replace what another installer owns).
func (u *Updater) binPath() (string, error) {
	p := u.Cfg.Bin
	if p == "" {
		return "", errors.New("install: no binary path (update.bin)")
	}
	st, err := os.Lstat(p)
	if err != nil {
		return "", fmt.Errorf("install: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("install: %s is not a regular file (a symlink? set update.bin to the real one)", p)
	}
	return p, nil
}

// preflight: the new binary prints its version and loads this warden.toml
// (`warden update --preflight`) before it replaces anything.
func (u *Updater) preflight(ctx context.Context, bin, tag string) error {
	args := []string{"update", "--preflight"}
	if u.ConfigPath != "" {
		args = append(args, "--config", u.ConfigPath)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := u.Exec(cctx, bin, args...)
	if err != nil {
		return fmt.Errorf("install: the new binary fails its preflight: %v %s", err, short(string(out), 300))
	}
	if v := strings.TrimPrefix(tag, "v"); !strings.Contains(string(out), "warden "+v) && !strings.Contains(string(out), "warden v"+v) {
		return fmt.Errorf("install: the new binary says %q, not %s", short(string(out), 80), tag)
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}
