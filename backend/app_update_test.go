// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BitBoxSwiss/bitbox-wallet-app/backend/appupdate"
	"github.com/BitBoxSwiss/bitbox02-api-go/util/semver"
	"github.com/stretchr/testify/require"
)

func waitForAppUpdate(t *testing.T, checker *updateChecker, status string) UpdateState {
	t.Helper()
	require.Eventually(t, func() bool { return checker.get().Installation.Status == status }, time.Second, time.Millisecond)
	return checker.get()
}

// Duplicate downloads share a frozen release, then a successful handoff starts installation.
func TestAppUpdateLifecycle(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var calls atomic.Int32
	installed := false
	checker := &updateChecker{
		latest: &UpdateFile{NewVersion: semver.NewSemVer(4, 53, 0)}, installation: &AppUpdateState{Status: "available"},
		download: func(ctx context.Context, _, version string) (*appupdate.Download, error) {
			calls.Add(1)
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &appupdate.Download{Version: version}, nil
		},
		install: func(*appupdate.Download, appupdate.InstallMessages) error {
			installed = true
			return nil
		},
	}
	t.Cleanup(checker.stop)
	require.True(t, checker.startDownload().Success)
	<-started
	initial := checker.get()
	require.Equal(t, "downloading", initial.Installation.Status)
	require.True(t, checker.startDownload().Success)
	require.Equal(t, int32(1), calls.Load())
	checker.set(&UpdateFile{NewVersion: semver.NewSemVer(4, 54, 0)})
	require.Equal(t, "4.53.0", checker.get().Update.NewVersion.String())
	close(release)
	ready := waitForAppUpdate(t, checker, "ready")
	require.Greater(t, ready.Revision, initial.Revision)
	// Previously emitted snapshots cannot mutate when a later state arrives.
	require.Equal(t, "downloading", initial.Installation.Status)
	require.True(t, checker.installUpdate(appupdate.InstallMessages{}).Success)
	require.True(t, installed)
	require.Equal(t, "installing", checker.get().Installation.Status)
}

// Closing the app cancels in-flight network work and waits for staging cleanup before returning.
func TestAppUpdateShutdownCancelsDownload(t *testing.T) {
	var canceled atomic.Bool
	checker := &updateChecker{
		latest: &UpdateFile{NewVersion: semver.NewSemVer(4, 53, 0)}, installation: &AppUpdateState{Status: "available"},
		download: func(ctx context.Context, _, _ string) (*appupdate.Download, error) {
			<-ctx.Done()
			canceled.Store(true)
			return nil, ctx.Err()
		},
	}
	require.True(t, checker.startDownload().Success)
	checker.stop()
	require.True(t, canceled.Load(), "shutdown must wait for the download to stop")
	require.False(t, checker.startDownload().Success)
}

// Failed installation requires a fresh download. Duplicate handoffs share that attempt,
// and a late failure after shutdown cannot change the closed controller.
func TestAppUpdateInstallRetry(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	replacement := &appupdate.Download{Version: "4.53.0"}
	checker := &updateChecker{
		latest: &UpdateFile{NewVersion: semver.NewSemVer(4, 53, 0)}, installation: &AppUpdateState{Status: "ready"},
		prepared: &appupdate.Download{Version: "4.53.0"},
		download: func(context.Context, string, string) (*appupdate.Download, error) {
			return replacement, nil
		},
		install: func(*appupdate.Download, appupdate.InstallMessages) error {
			if calls.Add(1) == 1 {
				return errors.New("could not start helper")
			}
			close(started)
			<-release
			return errors.New("helper startup failed after shutdown")
		},
	}
	t.Cleanup(checker.stop)
	require.Equal(t, "install", checker.installUpdate(appupdate.InstallMessages{}).ErrorCode)
	require.False(t, checker.installUpdate(appupdate.InstallMessages{}).Success)
	require.True(t, checker.startDownload().Success)
	waitForAppUpdate(t, checker, "ready")
	require.Same(t, replacement, checker.prepared)
	done := make(chan AppUpdateResult, 1)
	go func() { done <- checker.installUpdate(appupdate.InstallMessages{}) }()
	<-started
	require.True(t, checker.installUpdate(appupdate.InstallMessages{}).Success)
	require.Equal(t, int32(2), calls.Load())
	checker.stop()
	closedState := checker.get()
	close(release)
	require.Equal(t, "install", (<-done).ErrorCode)
	require.Equal(t, closedState, checker.get())
}
