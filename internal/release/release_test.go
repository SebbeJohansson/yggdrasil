package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	kind       byte
	mode       int64
}

func bundle(t *testing.T, entries []entry) string {
	t.Helper()
	var buf bytes.Buffer
	zipped := gzip.NewWriter(&buf)
	archive := tar.NewWriter(zipped)
	for _, e := range entries {
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: e.name, Typeflag: kind, Mode: mode, Size: int64(len(e.body))}
		if kind == tar.TypeSymlink {
			header.Linkname, header.Size = e.body, 0
		}
		if kind == tar.TypeDir {
			header.Size = 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := archive.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	archive.Close()
	zipped.Close()
	p := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnpackKeepsWhatTheBundleDoesNotHold(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".env"), []byte("COMPOSE_PROFILES=gjallar\n"), 0o600)
	os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("old"), 0o644)
	p := bundle(t, []entry{
		{name: "yggdrasil/", kind: tar.TypeDir},
		{name: "yggdrasil/VERSION", body: "v1.2.3\n"},
		{name: "yggdrasil/bin/ygg", body: "binary", mode: 0o755},
		{name: "yggdrasil/compose.yaml", body: "new"},
	})
	version, err := Unpack(p, root)
	if err != nil || version != "v1.2.3" {
		t.Fatalf("version %q, err %v", version, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "compose.yaml")); string(data) != "new" {
		t.Errorf("compose.yaml = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(root, ".env")); !strings.Contains(string(data), "gjallar") {
		t.Errorf(".env was touched: %q", data)
	}
	if info, err := os.Stat(filepath.Join(root, "bin/ygg")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("bin/ygg not executable: %v %v", info, err)
	}
}

func TestUnpackRefusesWhatLeavesTheFolder(t *testing.T) {
	for name, entries := range map[string][]entry{
		"dot-dot":     {{name: "yggdrasil/../evil", body: "x"}},
		"deep":        {{name: "yggdrasil/a/../../evil", body: "x"}},
		"absolute":    {{name: "/etc/evil", body: "x"}},
		"other":       {{name: "other/file", body: "x"}},
		"symlink":     {{name: "yggdrasil/link", body: "/etc/passwd", kind: tar.TypeSymlink}},
		"no version":  {{name: "yggdrasil/bin/ygg", body: "x"}},
		"bad version": {{name: "yggdrasil/VERSION", body: "latest"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Unpack(bundle(t, entries), t.TempDir()); err == nil {
				t.Fatal("unpacked")
			}
		})
	}
}

func TestVerifyMatchesSHA256SUMS(t *testing.T) {
	p := bundle(t, []entry{{name: "yggdrasil/VERSION", body: "v1.0.0"}})
	sum, _ := Sum(p)
	sums := filepath.Join(t.TempDir(), "SHA256SUMS")
	os.WriteFile(sums, []byte(sum+"  yggdrasil-v1.0.0-linux-amd64.tar.gz\n0000  other.tar.gz\n"), 0o600)
	if err := Verify(p, sums, "yggdrasil-v1.0.0-linux-amd64.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, sums, "other.tar.gz"); err == nil {
		t.Fatal("a wrong sum passed")
	}
	if err := Verify(p, sums, "missing.tar.gz"); err == nil {
		t.Fatal("a name not in SHA256SUMS passed")
	}
}

func TestFetchAndNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ok" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("hello"))
	}))
	defer server.Close()
	dst := filepath.Join(t.TempDir(), "f")
	if err := Fetch(context.Background(), server.Client(), server.URL+"/ok", dst); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(context.Background(), server.Client(), server.URL+"/missing", dst); err == nil {
		t.Fatal("a 404 fetched")
	}
	if Asset("v1.2.3", "arm64") != "yggdrasil-v1.2.3-linux-arm64.tar.gz" {
		t.Error(Asset("v1.2.3", "arm64"))
	}
	if URL("v1.2.3", "SHA256SUMS") != "https://github.com/SebbeJohansson/yggdrasil/releases/download/v1.2.3/SHA256SUMS" {
		t.Error(URL("v1.2.3", "SHA256SUMS"))
	}
	for v, ok := range map[string]bool{"v1.2.3": true, "v0.1.0-rc.1": true, "1.2.3": false, "v1.2": false, "latest": false, "v1.2.3;rm": false} {
		if ValidVersion(v) != ok {
			t.Errorf("ValidVersion(%q) = %v", v, !ok)
		}
	}
}

func TestABadBundleLeavesTheInstallAsItWas(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("old"), 0o644)
	p := bundle(t, []entry{{name: "yggdrasil/compose.yaml", body: "new"}, {name: "yggdrasil/VERSION", body: "nope"}})
	if _, err := Unpack(p, root); err == nil {
		t.Fatal("unpacked")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "compose.yaml")); string(data) != "old" {
		t.Errorf("compose.yaml = %q", data)
	}
	if left, _ := filepath.Glob(filepath.Join(root, ".unpack-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}
