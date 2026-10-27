package registry

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nenkoru/warden/internal/version"
)

const (
	maxMeta    = 32 << 20
	maxArchive = 512 << 20
	maxFile    = 256 << 20
	maxTotal   = 1 << 30
	maxEntries = 100_000
)

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// safe turns s into one path element.
func safe(s string) string {
	s = strings.TrimLeft(unsafeChars.ReplaceAllString(s, "_"), ".")
	if s == "" {
		return "_"
	}
	if len(s) > 120 {
		sum := sha256.Sum256([]byte(s))
		s = s[:100] + "-" + hex.EncodeToString(sum[:6])
	}
	return s
}

func (c *Client) get(ctx context.Context, method, u string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	for k, v := range hdr {
		req.Header[k] = v
	}
	return c.HTTP.Do(req)
}

func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	resp, err := c.get(ctx, "GET", u, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxMeta)).Decode(v)
}

func newHash(integrity string) (hash.Hash, func([]byte) string, error) {
	switch {
	case strings.HasPrefix(integrity, "sha512-"):
		return sha512.New(), func(b []byte) string { return "sha512-" + base64.StdEncoding.EncodeToString(b) }, nil
	case strings.HasPrefix(integrity, "sha256:"):
		return sha256.New(), func(b []byte) string { return "sha256:" + hex.EncodeToString(b) }, nil
	}
	return nil, nil, fmt.Errorf("unsupported integrity %q", integrity)
}

// pickSRI returns the sha512 entry of an SRI string ("sha512-… sha1-…").
func pickSRI(s string) string {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "sha512-") {
			return f
		}
	}
	return ""
}

// download fetches u into dst and checks it against integrity before keeping it.
func (c *Client) download(ctx context.Context, u, dst, integrity string) error {
	h, enc, err := newHash(integrity)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("refusing download URL %q", u)
	}
	resp, err := c.get(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArchive+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxArchive {
		err = fmt.Errorf("%s: larger than %d bytes", u, maxArchive)
	}
	if got := enc(h.Sum(nil)); err == nil && got != integrity {
		err = fmt.Errorf("%s: integrity mismatch: got %s, want %s", u, got, integrity)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func fileDigest(p, integrity string) (string, error) {
	h, enc, err := newHash(integrity)
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return enc(h.Sum(nil)), nil
}

// unpack extracts a .tar.gz or .zip into dst (replaced) and returns the content
// root: dst, or its single top-level directory (npm's "package/").
// Only regular files and dirs are written; escaping names are refused.
func unpack(archive, dst string) (string, error) {
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", err
	}
	var total int64
	entries := 0
	put := func(name string, isDir bool, mode fs.FileMode, r io.Reader) error {
		if entries++; entries > maxEntries {
			return errors.New("too many entries")
		}
		p, err := within(dst, name)
		if err != nil {
			return err
		}
		if p == dst {
			return nil
		}
		if isDir {
			return os.MkdirAll(p, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		perm := fs.FileMode(0o644)
		if mode&0o111 != 0 {
			perm = 0o755
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, io.LimitReader(r, maxFile+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if total += n; err == nil && (n > maxFile || total > maxTotal) {
			err = fmt.Errorf("entry %q: size limit", name)
		}
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return "", fmt.Errorf("%s: %w", archive, err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return "", err
	}
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return "", err
		}
		tr := tar.NewReader(gz)
		for {
			hd, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			switch hd.Typeflag {
			case tar.TypeDir, tar.TypeReg: // tar.Reader already turns the legacy TypeRegA into these
				if err := put(hd.Name, hd.Typeflag == tar.TypeDir, fs.FileMode(hd.Mode), tr); err != nil {
					return "", err
				}
			case tar.TypeXGlobalHeader, tar.TypeXHeader:
			default: // links and devices are skipped, but their names must be local too
				if _, err := within(dst, hd.Name); err != nil {
					return "", err
				}
			}
		}
	case bytes.Equal(magic, []byte("PK\x03\x04")):
		st, err := f.Stat()
		if err != nil {
			return "", err
		}
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			return "", err
		}
		for _, zf := range zr.File {
			m := zf.Mode()
			if !m.IsDir() && !m.IsRegular() {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return "", err
			}
			err = put(zf.Name, m.IsDir(), m, rc)
			rc.Close()
			if err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("%s: not a .tar.gz or .zip", archive)
	}
	des, err := os.ReadDir(dst)
	if err != nil {
		return "", err
	}
	if len(des) == 1 && des[0].IsDir() {
		return filepath.Join(dst, des[0].Name()), nil
	}
	return dst, nil
}

// sameTree re-extracts archive and compares it with the unpacked dir.
func sameTree(archive, dir string) error {
	tmp, err := os.MkdirTemp("", "warden-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	root, err := unpack(archive, filepath.Join(tmp, "x"))
	if err != nil {
		return err
	}
	want, err := treeHash(root)
	if err != nil {
		return err
	}
	got, err := treeHash(dir)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s differs from %s", dir, filepath.Base(archive))
	}
	return nil
}

// treeHash is "sha256:" over sorted lines kind\0relpath\0sha256(content),
// kind f|x (executable)|l (symlink, content = target). ".git" is skipped.
func treeHash(root string) (string, error) {
	st, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s: not a directory", root)
	}
	type ent struct{ rel, line string }
	var ents []ent
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		h := sha256.New()
		kind := "f"
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			kind = "l"
			h.Write([]byte(t))
		case info.Mode().IsRegular():
			if info.Mode()&0o111 != 0 {
				kind = "x"
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
		default:
			return nil
		}
		ents = append(ents, ent{rel, kind + "\x00" + rel + "\x00" + hex.EncodeToString(h.Sum(nil)) + "\n"})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].rel < ents[j].rel })
	h := sha256.New()
	for _, e := range ents {
		h.Write([]byte(e.line))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// within joins rel onto base and refuses absolute or escaping paths.
func within(base, rel string) (string, error) {
	r := strings.ReplaceAll(rel, "\\", "/")
	c := path.Clean(r)
	if c == "." {
		return base, nil
	}
	if path.IsAbs(r) || !filepath.IsLocal(filepath.FromSlash(c)) {
		return "", fmt.Errorf("path %q escapes %s", rel, base)
	}
	return filepath.Join(base, filepath.FromSlash(c)), nil
}
