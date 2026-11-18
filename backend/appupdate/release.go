// SPDX-License-Identifier: Apache-2.0

// Package appupdate downloads desktop release artifacts for the native installer.
package appupdate

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/BitBoxSwiss/bitbox-wallet-app/util/errp"
	"github.com/BitBoxSwiss/bitbox02-api-go/util/semver"
)

const (
	releaseDownloads = "https://github.com/BitBoxSwiss/bitbox-wallet-app/releases/download/v"
	maxArtifactSize  = 1 << 30
)

// Package identifies the native desktop installer format.
type Package string

const (
	// PackageDMG is the universal macOS disk image.
	PackageDMG Package = "dmg"
	// PackageWindows is the 64-bit Windows installer.
	PackageWindows Package = "windows"
)

// Downloader uses the caller's HTTP transport, including its proxy configuration.
type Downloader struct {
	client      *http.Client
	userAgent   string
	downloadURL string
}

// NewDownloader restricts redirects to GitHub's HTTPS release servers.
func NewDownloader(client *http.Client, userAgent string) *Downloader {
	copyClient := *client
	copyClient.Timeout = 20 * time.Minute
	copyClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errp.New("too many release download redirects")
		}
		switch request.URL.Host {
		case "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		default:
			return errp.New("unexpected release download host")
		}
		if request.URL.Scheme != "https" || request.URL.User != nil {
			return errp.New("insecure release download redirect")
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(request, via)
		}
		return nil
	}
	return &Downloader{client: &copyClient, userAgent: userAgent, downloadURL: releaseDownloads}
}

// Download owns a private staging directory. The native installer must verify the
// artifact's code signature, publisher and version before closing the running app.
type Download struct {
	Path    string
	Version string
	dir     string
}

// Cleanup removes only the staging directory created for this download.
func (d *Download) Cleanup() error { return os.RemoveAll(d.dir) }

// Download fetches the exact version announced by the update feed. Failed and
// canceled downloads are removed; the caller owns successful downloads and cleanup.
func (d *Downloader) Download(ctx context.Context, currentVersion, newVersion string, pkg Package, stagingParent string) (*Download, error) {
	current, err := parseVersion(currentVersion)
	if err != nil {
		return nil, err
	}
	next, err := parseVersion(newVersion)
	if err != nil {
		return nil, err
	}
	if current.AtLeast(next) {
		return nil, errp.New("release must be newer than the installed version")
	}
	var suffix string
	switch pkg {
	case PackageDMG:
		suffix = "macOS.dmg"
	case PackageWindows:
		suffix = "win64-installer.exe"
	default:
		return nil, errp.New("unsupported release package")
	}
	name := "BitBox-" + newVersion + "-" + suffix
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.downloadURL+newVersion+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", d.userAgent)
	response, err := d.client.Do(request)
	if err != nil {
		return nil, errp.Wrap(err, "request release")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errp.Newf("release request returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength == 0 || response.ContentLength > maxArtifactSize {
		return nil, errp.New("invalid release asset size")
	}
	dir, err := os.MkdirTemp(stagingParent, "bitbox-update-")
	if err != nil {
		return nil, err
	}
	result := &Download{Path: filepath.Join(dir, name), Version: newVersion, dir: dir}
	complete := false
	defer func() {
		if !complete {
			_ = result.Cleanup()
		}
	}()
	file, err := os.OpenFile(result.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	written, err := io.Copy(file, io.LimitReader(response.Body, maxArtifactSize+1))
	if err != nil {
		return nil, errp.Wrap(err, "download release file")
	}
	if written == 0 || written > maxArtifactSize || (response.ContentLength >= 0 && written != response.ContentLength) {
		return nil, errp.New("release asset is empty, truncated or oversized")
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	complete = true
	return result, nil
}

func parseVersion(value string) (*semver.SemVer, error) {
	version, err := semver.NewSemVerFromString(value)
	if err != nil || version.String() != value {
		return nil, errp.New("invalid stable release version")
	}
	return version, nil
}
