// Package release fetches, checks and unpacks a Yggdrasil release bundle
// (scripts/release-bundle.sh, docs/releases.md), for `ygg upgrade`.
//
// A bundle is yggdrasil-<version>-linux-<arch>.tar.gz with one top folder,
// yggdrasil/, beside a SHA256SUMS that names every bundle of the release. The
// bundle is checked against SHA256SUMS before a byte of it is unpacked, and
// unpacked only as plain files and folders under the install's root.
package release

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Repo is where releases are published.
const Repo = "SebbeJohansson/yggdrasil"

// MaxBundle bounds a download, and what one bundle may unpack to.
const MaxBundle = 512 << 20

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// ValidVersion reports whether v is a release version: v1.2.3, or v1.2.3-rc.1.
func ValidVersion(v string) bool {
	return versionPattern.MatchString(v)
}

// Asset is the bundle's file name for version and arch (amd64, arm64).
func Asset(version, arch string) string {
	return fmt.Sprintf("yggdrasil-%s-linux-%s.tar.gz", version, arch)
}

// URL is where a release's file is downloaded from.
func URL(version, file string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", Repo, version, file)
}

// Fetch downloads url into dst, refusing anything larger than MaxBundle.
func Fetch(ctx context.Context, client *http.Client, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, res.Status)
	}
	file, err := os.Create(dst)
	if err != nil {
		return err
	}
	n, err := io.Copy(file, io.LimitReader(res.Body, MaxBundle+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > MaxBundle {
		err = fmt.Errorf("%s: larger than %d bytes", url, MaxBundle)
	}
	return err
}

// Sum is the SHA-256 of the file at p, in hex.
func Sum(p string) (string, error) {
	file, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Expected is the sum SHA256SUMS (as `sha256sum` writes it) gives name.
func Expected(sums io.Reader, name string) (string, error) {
	scanner := bufio.NewScanner(sums)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("SHA256SUMS does not name %s", name)
}

// Verify checks the bundle at p against SHA256SUMS at sums, by name.
func Verify(p, sums, name string) error {
	list, err := os.Open(sums)
	if err != nil {
		return err
	}
	defer list.Close()
	want, err := Expected(list, name)
	if err != nil {
		return err
	}
	got, err := Sum(p)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: checksum %s, but SHA256SUMS says %s", name, got, want)
	}
	return nil
}

// Unpack writes the bundle at p into root, the top folder (yggdrasil/)
// stripped: regular files and folders only, every path inside root, modes
// kept. Files already in root and not in the bundle (data, profile, .env)
// stay as they are. It returns the bundle's VERSION.
//
// The bundle is unpacked into a folder beside root's files first and moved
// in only once all of it unpacked and its VERSION is a release's, so a bad
// bundle leaves the install as it was.
func Unpack(p, root string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(root, ".unpack-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	version, err := extract(p, stage)
	if err != nil {
		return "", err
	}
	return version, moveInto(stage, root)
}

// moveInto renames every file under stage to the same place under root.
func moveInto(stage, root string) error {
	return filepath.WalkDir(stage, func(p string, entry os.DirEntry, err error) error {
		if err != nil || p == stage {
			return err
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return os.Rename(p, target)
	})
}

// extract unpacks the bundle at p into dir, checked as Unpack says.
func extract(p, root string) (string, error) {
	file, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer file.Close()
	unzipped, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer unzipped.Close()
	archive := tar.NewReader(unzipped)
	var written int64
	version := ""
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name, ok := inside(header.Name)
		if !ok {
			return "", fmt.Errorf("bundle entry %q is outside its folder", header.Name)
		}
		if name == "" {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			written += header.Size
			if written > MaxBundle {
				return "", fmt.Errorf("bundle unpacks to more than %d bytes", MaxBundle)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			if err := writeFile(target, archive, os.FileMode(header.Mode)&0o755|0o600); err != nil {
				return "", err
			}
			if name == "VERSION" {
				data, _ := os.ReadFile(target)
				version = strings.TrimSpace(string(data))
			}
		default:
			return "", fmt.Errorf("bundle entry %q is not a plain file or folder", header.Name)
		}
	}
	if !ValidVersion(version) {
		return "", fmt.Errorf("bundle has no VERSION, or not a release version: %q", version)
	}
	return version, nil
}

// inside is the entry's path below the bundle's top folder, cleaned, and
// whether it stays below it.
func inside(name string) (string, bool) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	top, rest, found := strings.Cut(clean, "/")
	if top != "yggdrasil" {
		return "", false
	}
	if !found {
		return "", true
	}
	if rest == "" || rest == ".." || strings.HasPrefix(rest, "../") || path.IsAbs(rest) {
		return "", false
	}
	return rest, true
}

// writeFile writes a file of the staged bundle.
func writeFile(target string, from io.Reader, mode os.FileMode) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, from); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}
