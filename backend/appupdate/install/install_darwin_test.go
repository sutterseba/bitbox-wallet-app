// SPDX-License-Identifier: Apache-2.0

package install

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A failed second rename restores the complete old bundle at its original launch path.
func TestSwapBundleRestoresAfterFailure(t *testing.T) {
	dir := t.TempDir()
	source, target, backup := filepath.Join(dir, "new.app"), filepath.Join(dir, "BitBox.app"), filepath.Join(dir, "backup.app")
	require.NoError(t, os.Mkdir(source, 0700))
	require.NoError(t, os.Mkdir(target, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(target, "version"), []byte("old"), 0600))
	failure := errors.New("filesystem refused replacement")
	err := swapBundle(source, target, backup, func(from, to string) error {
		if from == source {
			return failure
		}
		return os.Rename(from, to)
	})
	require.ErrorIs(t, err, failure)
	previous, err := os.ReadFile(filepath.Join(target, "version"))
	require.NoError(t, err)
	require.Equal(t, "old", string(previous))
}

// Neither an unsigned bundle nor a valid ad-hoc signature can impersonate the release publisher.
func TestRejectUntrustedBundle(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	bundle := filepath.Join(dir, "BitBox.app")
	require.NoError(t, os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "Contents", "MacOS", "BitBox"), []byte("unsigned"), 0700))
	plist := `<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>ch.shiftcrypto.wallet</string><key>CFBundleShortVersionString</key><string>4.53.0</string></dict></plist>`
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(plist), 0600))
	require.ErrorContains(t, validateBundle(bundle, "4.53.0"), "invalid application code signature")
	require.ErrorContains(t, validateBundle(bundle, "4.54.0"), "version does not match")
	executable := filepath.Join(bundle, "Contents", "MacOS", "BitBox")
	require.NoError(t, os.Remove(executable))
	require.NoError(t, copyFile("/usr/bin/true", executable))
	output, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", bundle).CombinedOutput()
	require.NoError(t, err, string(output))
	output, err = exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", bundle).CombinedOutput()
	require.NoError(t, err, string(output))
	require.ErrorContains(t, validateBundle(bundle, "4.53.0"), "invalid application code signature")
}

// With a real release supplied, exercise signature verification, DMG mounting, copying and
// replacement in a scratch bundle. This deliberately does not launch a wallet or use its data.
func TestReleaseDMGInstall(t *testing.T) {
	artifact := os.Getenv("BITBOX_UPDATE_TEST_DMG")
	if artifact == "" {
		t.Skip("set BITBOX_UPDATE_TEST_DMG to an official release DMG")
	}
	match := regexp.MustCompile(`^BitBox-([0-9]+\.[0-9]+\.[0-9]+)-macOS\.dmg$`).FindStringSubmatch(filepath.Base(artifact))
	require.Len(t, match, 2)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	bundle := filepath.Join(dir, "BitBox.app")
	executable := filepath.Join(bundle, "Contents", "MacOS", "BitBox")
	require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0700))
	require.NoError(t, os.WriteFile(executable, []byte("old app placeholder"), 0700))
	plist := `<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>ch.shiftcrypto.wallet</string><key>CFBundleShortVersionString</key><string>0.0.1</string></dict></plist>`
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(plist), 0600))
	stage := filepath.Join(dir, "installer")
	require.NoError(t, os.Mkdir(stage, 0700))
	m := manifest{Artifact: artifact, Version: match[1], Executable: executable, PID: os.Getpid()}
	apply, cleanup, err := prepare(m, stage)
	require.NoError(t, err)
	defer cleanup()
	require.ErrorContains(t, validateTarget(filepath.Join(stage, "mount", "BitBox.app", "Contents", "MacOS", "BitBox")), "writable volume")
	require.NoError(t, apply())
	require.NoError(t, validateBundle(bundle, match[1]))
}

// A native socket without CLOEXEC must be closed in the helper, or the restarted Qt app
// inherits an orphaned listener and cannot bind its own debugger or local IPC endpoint.
func TestCloseInheritedDescriptors(t *testing.T) {
	if inherited := os.Getenv("BITBOX_UPDATE_TEST_INHERITED_FD"); inherited != "" {
		fd, err := strconv.Atoi(inherited)
		require.NoError(t, err)
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		require.NoError(t, err, "the test must reproduce an inherited native descriptor")
		require.Zero(t, flags&unix.FD_CLOEXEC)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer listener.Close()
		timer := time.NewTimer(time.Millisecond)
		defer timer.Stop()
		require.NoError(t, closeInheritedDescriptors())
		_, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		require.ErrorIs(t, err, unix.EBADF)
		// The existing Go poller and timer must keep working, while stdout is still
		// available for the updater's readiness handshake.
		accepted := make(chan error, 1)
		go func() {
			connection, err := listener.Accept()
			if err == nil {
				err = connection.Close()
			}
			accepted <- err
		}()
		connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		require.NoError(t, err)
		require.NoError(t, connection.Close())
		require.NoError(t, <-accepted)
		<-timer.C
		t.Log("runtime poller, timers and stdout preserved")
		return
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	require.NoError(t, err)
	defer unix.Close(fd)
	require.NoError(t, unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
	require.NoError(t, unix.Listen(fd, 1))
	_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0)
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCloseInheritedDescriptors$", "-test.v")
	cmd.Env = append(os.Environ(), "BITBOX_UPDATE_TEST_INHERITED_FD="+strconv.Itoa(fd))
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Contains(t, string(output), "runtime poller, timers and stdout preserved")
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	require.NoError(t, err, "the original process must retain its socket")
}
