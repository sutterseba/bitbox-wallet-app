// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/BitBoxSwiss/bitbox-wallet-app/backend/appupdate"
	"github.com/BitBoxSwiss/bitbox-wallet-app/backend/versioninfo"
	"github.com/BitBoxSwiss/bitbox-wallet-app/util/logging"
)

// AppUpdateConfig enables installation only in native environments with a packaged updater.
type AppUpdateConfig struct {
	Package appupdate.Package
	Install func(*appupdate.Download, appupdate.InstallMessages) error
}

// AppUpdateState describes a user-requested download and installation.
type AppUpdateState struct {
	Status    string `json:"status"`
	ErrorCode string `json:"errorCode,omitempty"`
}

// AppUpdateResult reports whether the requested action was accepted.
type AppUpdateResult struct {
	Success   bool   `json:"success"`
	ErrorCode string `json:"errorCode,omitempty"`
}

func (backend *Backend) configureAppUpdate() {
	if backend.environment == nil {
		return
	}
	config := backend.environment.AppUpdate()
	if config == nil || config.Install == nil {
		return
	}
	checker := backend.updateChecker
	checker.installation = &AppUpdateState{Status: "available"}
	checker.install = config.Install
	checker.download = func(ctx context.Context, current, version string) (*appupdate.Download, error) {
		client, err := backend.socksProxy.GetHTTPClient()
		if err != nil {
			return nil, err
		}
		defer client.CloseIdleConnections()
		downloader := appupdate.NewDownloader(client, backend.userAgent())
		staging := filepath.Join(backend.arguments.MainDirectoryPath(), "updates")
		if err := os.MkdirAll(staging, 0700); err != nil {
			return nil, err
		}
		if err := os.Chmod(staging, 0700); err != nil {
			return nil, err
		}
		return downloader.Download(ctx, current, version, config.Package, staging)
	}
}

func sameUpdateVersion(a, b *UpdateFile) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.NewVersion == nil || b.NewVersion == nil {
		return a.NewVersion == b.NewVersion
	}
	return a.NewVersion.String() == b.NewVersion.String()
}

// DownloadUpdate starts a single background download that survives frontend navigation.
func (backend *Backend) DownloadUpdate() AppUpdateResult {
	return backend.updateChecker.startDownload()
}

func (checker *updateChecker) startDownload() AppUpdateResult {
	unlock := checker.latestLock.Lock()
	if checker.closed || checker.download == nil || checker.latest == nil || checker.latest.NewVersion == nil {
		unlock()
		return AppUpdateResult{ErrorCode: "unavailable"}
	}
	if checker.prepared != nil || checker.installation.Status == "downloading" {
		unlock()
		return AppUpdateResult{Success: true}
	}
	version := checker.latest.NewVersion.String()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	checker.downloadCancel = cancel
	checker.installation = &AppUpdateState{Status: "downloading"}
	checker.revision++
	state := checker.snapshotLocked()
	checker.downloadDone.Add(1)
	unlock()
	checker.notifyUpdate(state)
	go func() {
		defer checker.downloadDone.Done()
		defer cancel()
		prepared, err := checker.download(ctx, versioninfo.Version.String(), version)
		unlock := checker.latestLock.Lock()
		checker.downloadCancel = nil
		if checker.closed {
			unlock()
			cleanupDownload(prepared)
			return
		}
		if err != nil {
			checker.installation = &AppUpdateState{Status: "error", ErrorCode: "download"}
		} else {
			checker.prepared = prepared
			checker.installation.Status = "ready"
		}
		checker.revision++
		state := checker.snapshotLocked()
		unlock()
		if err != nil {
			logging.Get().WithGroup("update").WithError(err).Warn("App update download failed")
			cleanupDownload(prepared)
		}
		checker.notifyUpdate(state)
	}()
	return AppUpdateResult{Success: true}
}

// InstallUpdate starts native verification; the app exits only after a successful handoff.
func (backend *Backend) InstallUpdate(messages appupdate.InstallMessages) AppUpdateResult {
	if !messages.Valid() {
		return AppUpdateResult{ErrorCode: "invalid-request"}
	}
	return backend.updateChecker.installUpdate(messages)
}

func (checker *updateChecker) installUpdate(messages appupdate.InstallMessages) AppUpdateResult {
	unlock := checker.latestLock.Lock()
	if checker.closed || checker.install == nil || checker.prepared == nil {
		unlock()
		return AppUpdateResult{ErrorCode: "unavailable"}
	}
	if checker.installation.Status == "installing" {
		unlock()
		return AppUpdateResult{Success: true}
	}
	prepared := checker.prepared
	checker.installation.Status = "installing"
	checker.installation.ErrorCode = ""
	checker.revision++
	state := checker.snapshotLocked()
	unlock()
	checker.notifyUpdate(state)
	if err := checker.install(prepared, messages); err != nil {
		logging.Get().WithGroup("update").WithError(err).Warn("App update installation could not start")
		unlock := checker.latestLock.Lock()
		if checker.closed {
			unlock()
			// Shutdown leaves staging to an in-flight installer. A failed handoff returns
			// ownership here, after shutdown's cleanup has already finished.
			cleanupDownload(prepared)
			return AppUpdateResult{ErrorCode: "install"}
		}
		checker.prepared = nil
		checker.installation.Status = "error"
		checker.installation.ErrorCode = "install"
		checker.revision++
		state := checker.snapshotLocked()
		unlock()
		cleanupDownload(prepared)
		checker.notifyUpdate(state)
		return AppUpdateResult{ErrorCode: "install"}
	}
	return AppUpdateResult{Success: true}
}

func (checker *updateChecker) stopDownload() {
	unlock := checker.latestLock.Lock()
	checker.closed = true
	if checker.downloadCancel != nil {
		checker.downloadCancel()
	}
	prepared := checker.prepared
	// The detached helper owns staging after installation starts.
	if checker.installation != nil && checker.installation.Status == "installing" {
		prepared = nil
	}
	unlock()
	checker.downloadDone.Wait()
	cleanupDownload(prepared)
}

func cleanupDownload(prepared *appupdate.Download) {
	if prepared != nil {
		if err := prepared.Cleanup(); err != nil {
			logging.Get().WithGroup("update").WithError(err).Warn("Could not clean up downloaded app update")
		}
	}
}
