package main

// ygg upgrade: this machine to a release (docs/releases.md).
//
//	ygg upgrade v0.2.0                       download, check, unpack, install
//	ygg upgrade latest                       the newest release
//	ygg upgrade v0.2.0 --from bundle.tar.gz  a bundle copied here by hand,
//	                                         with SHA256SUMS beside it
//
// The bundle is checked against the release's SHA256SUMS, unpacked into the
// install (YGG_HOME, default ~/.local/share/yggdrasil) over the previous
// release's files, keeping data/, profile/ and .env, and its install.sh run:
// the same profile, the new ygg and the release's image for the containers.
// Going back is the same command with the older version.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"yggdrasil/internal/part"
	"yggdrasil/internal/release"
)

// installHome is where a release is installed.
func installHome() (string, error) {
	if home := os.Getenv("YGG_HOME"); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local/share/yggdrasil"), nil
}

// latestVersion is the newest release's tag.
func latestVersion(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+release.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the latest release: %s", res.Status)
	}
	var body struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", err
	}
	if !release.ValidVersion(body.Tag) {
		return "", fmt.Errorf("the latest release is tagged %q, not a version", body.Tag)
	}
	return body.Tag, nil
}

func upgradeCommand(args []string) int {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	from := flags.String("from", "", "a bundle on disk instead of downloading it")
	sums := flags.String("sums", "", "its SHA256SUMS (default: beside the bundle)")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: ygg upgrade <version>|latest [--from bundle.tar.gz [--sums SHA256SUMS]]")
		return 2
	}
	version := args[0]
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		fmt.Fprintf(os.Stderr, "ygg upgrade: releases are built for amd64 and arm64, not %s\n", arch)
		return 1
	}
	home, err := installHome()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ygg upgrade:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Minute}

	if version == "latest" {
		if *from != "" {
			fmt.Fprintln(os.Stderr, "ygg upgrade: name the version of a bundle given with --from")
			return 2
		}
		if version, err = latestVersion(ctx, client); err != nil {
			fmt.Fprintln(os.Stderr, "ygg upgrade:", err)
			return 1
		}
	}
	if !release.ValidVersion(version) {
		fmt.Fprintf(os.Stderr, "ygg upgrade: %q is not a version like v0.2.0\n", version)
		return 2
	}
	if version == part.Version {
		fmt.Printf("Already on %s; installing it again.\n", version)
	}

	if err := upgrade(ctx, client, version, arch, home, *from, *sums); err != nil {
		fmt.Fprintln(os.Stderr, "ygg upgrade:", err)
		return 1
	}
	return 0
}

func upgrade(ctx context.Context, client *http.Client, version, arch, home, from, sums string) error {
	asset := release.Asset(version, arch)
	bundle := from
	if bundle == "" {
		work, err := os.MkdirTemp("", "ygg-upgrade-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		bundle, sums = filepath.Join(work, asset), filepath.Join(work, "SHA256SUMS")
		fmt.Printf("Downloading %s...\n", asset)
		if err := release.Fetch(ctx, client, release.URL(version, "SHA256SUMS"), sums); err != nil {
			return err
		}
		if err := release.Fetch(ctx, client, release.URL(version, asset), bundle); err != nil {
			return err
		}
	} else if sums == "" {
		sums = filepath.Join(filepath.Dir(from), "SHA256SUMS")
	}
	if _, err := os.Stat(sums); err != nil {
		return fmt.Errorf("no SHA256SUMS to check the bundle against (%s): copy the release's SHA256SUMS beside it", sums)
	}
	// By the release's own name, so a bundle for another version or machine fails here.
	if err := release.Verify(bundle, sums, asset); err != nil {
		return err
	}
	fmt.Printf("Checked against SHA256SUMS. Unpacking into %s...\n", home)
	got, err := release.Unpack(bundle, home)
	if err != nil {
		return err
	}
	if got != version {
		return fmt.Errorf("the bundle says it is %s, not %s", got, version)
	}

	install := exec.CommandContext(ctx, "sh", filepath.Join(home, "scripts/install.sh"))
	install.Stdout, install.Stderr, install.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := install.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("install.sh failed (%d); the files of %s are in %s", exit.ExitCode(), version, home)
		}
		return err
	}
	return nil
}
