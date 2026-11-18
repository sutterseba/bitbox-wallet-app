// SPDX-License-Identifier: Apache-2.0

package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BitBoxSwiss/bitbox02-api-go/util/semver"
	"golang.org/x/sys/unix"
)

const bundleID = "ch.shiftcrypto.wallet"

func ignoreBrokenPipe() { signal.Ignore(syscall.SIGPIPE) }

// Qt can leave native sockets without close-on-exec. They must not keep the old app's
// listeners or connections alive through the helper and the subsequent relaunch.
func closeInheritedDescriptors() error {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if errors.Is(err, unix.EBADF) {
			continue
		}
		if err != nil {
			return err
		}
		// The Go runtime's own descriptors have CLOEXEC set. Preserve those and the
		// standard streams, including stdout used for the readiness handshake.
		if flags&unix.FD_CLOEXEC == 0 {
			if err := unix.Close(fd); err != nil && !errors.Is(err, unix.EBADF) {
				return err
			}
		}
	}
	return nil
}

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func bundlePath(executable string) (string, error) {
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(executable)))
	if !strings.HasSuffix(bundle, ".app") || filepath.Join(bundle, "Contents", "MacOS", "BitBox") != executable {
		return "", errors.New("updates require an installed BitBox.app bundle")
	}
	return bundle, nil
}

func validateApplication(executable string) error {
	if err := regularFile(executable); err != nil {
		return err
	}
	bundle, err := bundlePath(executable)
	if err != nil {
		return err
	}
	identifier, err := plistString(bundle, "CFBundleIdentifier")
	if err != nil || identifier != bundleID {
		return errors.New("installation is not the BitBoxApp bundle")
	}
	return nil
}

func validateTarget(executable string) error {
	if err := validateApplication(executable); err != nil {
		return err
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs(executable, &filesystem); err != nil {
		return err
	}
	if filesystem.Flags&unix.MNT_RDONLY != 0 || strings.Contains(executable, "/AppTranslocation/") {
		return errors.New("move BitBoxApp to a writable volume before updating")
	}
	return nil
}

func plistString(bundle, key string) (string, error) {
	out, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :"+key,
		filepath.Join(bundle, "Contents", "Info.plist")).Output()
	return strings.TrimSpace(string(out)), err
}

func validateBundle(bundle, version string) error {
	if err := validateApplication(filepath.Join(bundle, "Contents", "MacOS", "BitBox")); err != nil {
		return err
	}
	actualVersion, err := plistString(bundle, "CFBundleShortVersionString")
	if err != nil || actualVersion != version {
		return errors.New("downloaded app version does not match the selected release")
	}
	// Only a valid BitBoxApp signature from the release publisher is trusted.
	requirement := `identifier "ch.shiftcrypto.wallet" and anchor apple generic and certificate leaf[subject.OU] = "XK248TQN88"`
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", "-R", "="+requirement, bundle).CombinedOutput(); err != nil {
		return fmt.Errorf("invalid application code signature: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func prepare(m manifest, dir string) (func() error, func(), error) {
	mount := filepath.Join(dir, "mount")
	if err := os.Mkdir(mount, 0700); err != nil {
		return nil, nil, err
	}
	if out, err := exec.Command("/usr/bin/hdiutil", "attach", "-readonly", "-nobrowse", "-noautoopen",
		"-mountpoint", mount, m.Artifact).CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("mount update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	cleanup := func() {
		_ = exec.Command("/usr/bin/hdiutil", "detach", mount).Run()
		_ = os.RemoveAll(dir)
		_ = os.Remove(filepath.Dir(dir)) // Remove the download directory only if it is empty.
	}
	source := filepath.Join(mount, "BitBox.app")
	if err := validateBundle(source, m.Version); err != nil {
		cleanup()
		return nil, nil, err
	}
	apply := func() error {
		target, err := bundlePath(m.Executable)
		if err != nil {
			return err
		}
		currentVersion, err := plistString(target, "CFBundleShortVersionString")
		if err != nil {
			return err
		}
		current, err := semver.NewSemVerFromString(currentVersion)
		if err != nil {
			return err
		}
		next, err := semver.NewSemVerFromString(m.Version)
		if err != nil {
			return err
		}
		if current.AtLeast(next) {
			return errors.New("the installed app is already this version or newer")
		}
		// Only the replacement operation is elevated. The supervising helper remains the user
		// process and is responsible for relaunching the app without administrator privileges.
		probe, err := os.MkdirTemp(filepath.Dir(target), ".bitbox-permission-")
		if err == nil {
			_ = os.Remove(probe)
			return replaceBundle(source, target, m.Version)
		}
		if !errors.Is(err, os.ErrPermission) {
			return err
		}
		helper, err := os.Executable()
		if err != nil {
			return err
		}
		command := shellQuote(helper) + " --apply-macos --manifest " + shellQuote(filepath.Join(dir, "install.json"))
		out, err := exec.Command("/usr/bin/osascript", "-e",
			"on run argv\n do shell script (item 1 of argv) with administrator privileges\nend run", command).CombinedOutput()
		if err != nil {
			return fmt.Errorf("authorize update: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return nil
	}
	return apply, cleanup, nil
}

// ApplyMacOS performs the bundle replacement when invoked through macOS authorization.
func ApplyMacOS(path string) error {
	m, err := readManifest(path)
	if err != nil {
		return err
	}
	if err := m.unchanged(); err != nil {
		return err
	}
	target, err := bundlePath(m.Executable)
	if err != nil {
		return err
	}
	source := filepath.Join(filepath.Dir(path), "mount", "BitBox.app")
	if err := validateBundle(source, m.Version); err != nil {
		return err
	}
	return replaceBundle(source, target, m.Version)
}

func replaceBundle(source, target, version string) error {
	stage, err := os.MkdirTemp(filepath.Dir(target), ".bitbox-update-")
	if err != nil {
		return err
	}
	removeStage := true
	defer func() {
		if removeStage {
			_ = os.RemoveAll(stage)
		}
	}()
	newBundle := filepath.Join(stage, "BitBox.app")
	if out, err := exec.Command("/usr/bin/ditto", "--rsrc", "--extattr", source, newBundle).CombinedOutput(); err != nil {
		return fmt.Errorf("copy update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if err := validateBundle(newBundle, version); err != nil {
		return err
	}
	backup := filepath.Join(stage, "previous.app")
	err = swapBundle(newBundle, target, backup, os.Rename)
	if err != nil {
		// Preserve the previous bundle if restoring it also failed.
		if _, statErr := os.Stat(backup); statErr == nil {
			removeStage = false
		}
	}
	return err
}

func swapBundle(source, target, backup string, rename func(string, string) error) error {
	if err := rename(target, backup); err != nil {
		return err
	}
	if err := rename(source, target); err != nil {
		if restoreErr := rename(backup, target); restoreErr != nil {
			return fmt.Errorf("replace failed: %w; could not restore previous app at %s: %v", err, backup, restoreErr)
		}
		return err
	}
	return nil
}

func watchParent(pid int) (func() error, func(), error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, nil, err
	}
	closeWait := func() { _ = unix.Close(fd) }
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC,
		Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil); err != nil {
		closeWait()
		return nil, nil, err
	}
	return func() error {
		events := make([]unix.Kevent_t, 1)
		timeout := unix.NsecToTimespec(int64(2 * time.Minute))
		for {
			n, err := unix.Kevent(fd, nil, events, &timeout)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return err
			}
			if n == 0 {
				return errors.New("BitBoxApp did not exit; update was not installed")
			}
			return nil
		}
	}, closeWait, nil
}

func showError(message string) {
	_ = exec.Command("/usr/bin/osascript", "-e",
		`on run argv
display alert "BitBoxApp" message (item 1 of argv) as critical
end run`, message).Run()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
