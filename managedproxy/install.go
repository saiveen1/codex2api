package managedproxy

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

func executable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0
}

func (m *Manager) resolveBinary(ctx context.Context, c EngineConfig) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("managed proxy: owned process launch is supported only on Linux; Windows requires a WMI launcher, not os/exec")
	}
	if c.BinaryPath != "" {
		path, err := filepath.Abs(c.BinaryPath)
		if err != nil || !executable(path) {
			return "", errors.New("managed proxy: configured mihomo binary is missing or not executable")
		}
		return path, nil
	}
	if path, err := exec.LookPath("mihomo"); err == nil {
		if abs, err := filepath.Abs(path); err == nil && executable(abs) {
			return abs, nil
		}
	}
	owned := filepath.Join(m.root, "bin", "mihomo")
	if executable(owned) {
		return owned, nil
	}
	if !c.AutoInstall {
		return "", errors.New("managed proxy: mihomo not found; set binary_path or enable auto_install")
	}
	if err := installBinary(ctx, owned); err != nil {
		return "", err
	}
	return owned, nil
}

func chooseAsset(assets []releaseAsset, arch string) (releaseAsset, error) {
	want := "mihomo-linux-" + arch + "-"
	var candidates []releaseAsset
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if !strings.HasPrefix(name, want) || !(strings.HasSuffix(name, ".gz") || strings.HasSuffix(name, ".zip")) || strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		rest := strings.TrimPrefix(name, want)
		// amd64 compatible avoids silently installing AVX/SSE-specific builds.
		if arch == "amd64" && !strings.HasPrefix(rest, "compatible-") {
			continue
		}
		if arch != "amd64" && !strings.HasPrefix(rest, "v") {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return releaseAsset{}, errors.New("managed proxy: no compatible official Linux artifact; install mihomo manually and set binary_path")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	return candidates[0], nil
}

func officialAssetURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && strings.HasPrefix(u.Path, "/MetaCubeX/mihomo/releases/download/")
}

func installBinary(ctx context.Context, dest string) error {
	if runtime.GOOS != "linux" {
		return errors.New("managed proxy: automatic installation supports Linux only; provide a supported Linux host")
	}
	b, err := fetchRemote(ctx, "https://api.github.com/repos/MetaCubeX/mihomo/releases/latest", 4<<20)
	if err != nil {
		return errors.New("managed proxy: official release lookup failed; supply binary_path or retry later")
	}
	var release struct {
		Assets []releaseAsset `json:"assets"`
	}
	if json.Unmarshal(b, &release) != nil {
		return errors.New("managed proxy: invalid official release metadata")
	}
	a, err := chooseAsset(release.Assets, runtime.GOARCH)
	if err != nil {
		return err
	}
	if !officialAssetURL(a.URL) {
		return errors.New("managed proxy: invalid official asset URL")
	}
	digest := strings.TrimPrefix(a.Digest, "sha256:")
	if !strings.HasPrefix(a.Digest, "sha256:") || !hexID(digest, 64) {
		digest = ""
		for _, c := range release.Assets {
			name := strings.ToLower(c.Name)
			if !(strings.Contains(name, "checksum") || strings.Contains(name, "sha256")) || !officialAssetURL(c.URL) {
				continue
			}
			checksums, err := fetchRemote(ctx, c.URL, 2<<20)
			if err != nil {
				continue
			}
			digest = checksumFor(checksums, a.Name)
			if digest != "" {
				break
			}
		}
	}
	if !hexID(digest, 64) {
		return errors.New("managed proxy: official SHA256 is unavailable; refusing unverified install, supply binary_path")
	}
	compressed, err := fetchRemote(ctx, a.URL, 64<<20)
	if err != nil {
		return errors.New("managed proxy: official binary download failed")
	}
	sum := sha256.Sum256(compressed)
	if hex.EncodeToString(sum[:]) != digest {
		return errors.New("managed proxy: official binary SHA256 mismatch")
	}
	binary, err := unpackBinary(a.Name, compressed)
	if err != nil {
		return err
	}
	if len(binary) < 4 || !bytes.Equal(binary[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return errors.New("managed proxy: official artifact is not a Linux executable")
	}
	return atomicFile(dest, binary, 0700)
}

func checksumFor(b []byte, name string) string {
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name && hexID(strings.ToLower(fields[0]), 64) {
			return strings.ToLower(fields[0])
		}
		if len(fields) == 4 && fields[0] == "SHA256" && fields[1] == "("+name+")" && fields[2] == "=" && hexID(strings.ToLower(fields[3]), 64) {
			return strings.ToLower(fields[3])
		}
	}
	return ""
}

func unpackBinary(name string, b []byte) ([]byte, error) {
	var r io.ReadCloser
	if strings.HasSuffix(name, ".gz") {
		g, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, errors.New("managed proxy: invalid official gzip artifact")
		}
		r = g
	} else if strings.HasSuffix(name, ".zip") {
		z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return nil, errors.New("managed proxy: invalid official zip artifact")
		}
		for _, f := range z.File {
			if f.FileInfo().Mode().IsRegular() && filepath.Base(f.Name) == "mihomo" {
				if r != nil {
					r.Close()
					return nil, errors.New("managed proxy: ambiguous official zip artifact")
				}
				r, err = f.Open()
				if err != nil {
					return nil, errors.New("managed proxy: cannot unpack official artifact")
				}
			}
		}
	}
	if r == nil {
		return nil, errors.New("managed proxy: unsupported official archive; supply binary_path")
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, (128<<20)+1))
	if err != nil || len(out) == 0 || len(out) > 128<<20 {
		return nil, errors.New("managed proxy: corrupt or oversized official artifact")
	}
	return out, nil
}
