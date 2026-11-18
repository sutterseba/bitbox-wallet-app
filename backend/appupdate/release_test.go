// SPDX-License-Identifier: Apache-2.0

package appupdate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testDownloader(t *testing.T, handler http.HandlerFunc) *Downloader {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	d := NewDownloader(server.Client(), "updater-test")
	d.downloadURL = server.URL + "/download/v"
	return d
}

// Both platforms fetch only the announced version, keep downloads private, and clean up
// only their own staging directory. Native signature verification belongs to the installer.
func TestDownload(t *testing.T) {
	for pkg, name := range map[Package]string{
		PackageDMG: "BitBox-4.54.0-macOS.dmg", PackageWindows: "BitBox-4.54.0-win64-installer.exe",
	} {
		t.Run(string(pkg), func(t *testing.T) {
			d := testDownloader(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/download/v4.54.0/"+name, r.URL.Path)
				_, _ = w.Write([]byte("installer contents"))
			})
			parent := t.TempDir()
			sentinel := filepath.Join(parent, "unrelated")
			require.NoError(t, os.WriteFile(sentinel, nil, 0600))
			download, err := d.Download(context.Background(), "4.53.0", "4.54.0", pkg, parent)
			require.NoError(t, err)
			data, err := os.ReadFile(download.Path)
			require.NoError(t, err)
			require.Equal(t, "installer contents", string(data))
			if runtime.GOOS != "windows" {
				for path, mode := range map[string]os.FileMode{download.dir: 0700, download.Path: 0600} {
					info, err := os.Stat(path)
					require.NoError(t, err)
					require.Equal(t, mode, info.Mode().Perm())
				}
			}
			require.NoError(t, download.Cleanup())
			require.NoDirExists(t, download.dir)
			require.FileExists(t, sentinel)
		})
	}
}

// Downgrades, ambiguous versions and unsupported packages are rejected before network I/O.
func TestDownloadRejectsVersions(t *testing.T) {
	d := testDownloader(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected download") })
	for _, versions := range [][2]string{{"4.54.0", "4.54.0"}, {"4.55.0", "4.54.0"}, {"4.53.0", "4.54.0-rc1"},
		{"4.53.0", "65536.0.0"}, {"4.53.0", "../4.54.0"}} {
		t.Run(fmt.Sprint(versions), func(t *testing.T) {
			_, err := d.Download(context.Background(), versions[0], versions[1], PackageDMG, t.TempDir())
			require.Error(t, err)
		})
	}
	_, err := d.Download(context.Background(), "4.53.0", "4.54.0", Package("apk"), t.TempDir())
	require.Error(t, err)
}

// Canceling a streamed download interrupts the transfer and removes its staging files.
func TestDownloadCancellation(t *testing.T) {
	started := make(chan struct{})
	d := testDownloader(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial download"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parent := t.TempDir()
	finished := make(chan error, 1)
	go func() {
		_, err := d.Download(ctx, "4.53.0", "4.54.0", PackageDMG, parent)
		finished <- err
	}()
	<-started
	require.Eventually(t, func() bool {
		paths, _ := filepath.Glob(filepath.Join(parent, "*", "*"))
		return len(paths) == 1
	}, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-finished, context.Canceled)
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// GitHub's HTTPS CDN is allowed; redirecting to HTTP, credentials or a local host is not.
func TestDownloadRedirectPolicy(t *testing.T) {
	d := NewDownloader(http.DefaultClient, "test")
	for _, url := range []string{"https://release-assets.githubusercontent.com/file", "https://objects.githubusercontent.com/file"} {
		r, err := http.NewRequest(http.MethodGet, url, nil)
		require.NoError(t, err)
		require.NoError(t, d.client.CheckRedirect(r, nil))
	}
	for _, url := range []string{"http://github.com/file", "https://github.com.evil.invalid/file", "http://127.0.0.1/file", "https://user:pass@github.com/file"} {
		r, err := http.NewRequest(http.MethodGet, url, nil)
		require.NoError(t, err)
		require.Error(t, d.client.CheckRedirect(r, nil))
	}
}
