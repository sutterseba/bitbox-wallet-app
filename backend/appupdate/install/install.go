// SPDX-License-Identifier: Apache-2.0

// Package install verifies and installs desktop updates in an independent process.
package install

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/BitBoxSwiss/bitbox-wallet-app/backend/appupdate"
)

const helperName = "bitbox-app-update"

type manifest struct {
	Artifact      string                    `json:"artifact"`
	Version       string                    `json:"version"`
	PID           int                       `json:"pid"`
	Executable    string                    `json:"executable"`
	Arguments     []string                  `json:"arguments"`
	CurrentDigest string                    `json:"currentDigest"`
	Messages      appupdate.InstallMessages `json:"messages"`
}

type response struct {
	Ready bool   `json:"ready"`
	Error string `json:"error,omitempty"`
}

func helperPath(executable string) string {
	name := helperName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(executable), name)
}

// Available reports whether this is an installed desktop application with its updater.
func Available() bool {
	executable, err := os.Executable()
	if err != nil || validateTarget(executable) != nil {
		return false
	}
	return regularFile(helperPath(executable)) == nil
}

// Start prepares an independent installer and returns only once it can safely wait for shutdown.
// The caller must quit the application normally after Start succeeds.
func Start(artifactPath, version string, args []string, messages appupdate.InstallMessages) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	digest, err := installationDigest(executable)
	if err != nil {
		return err
	}
	m := manifest{Artifact: artifactPath, Version: version,
		PID: os.Getpid(), Executable: executable, Arguments: args, CurrentDigest: digest, Messages: messages}
	if err := m.validate(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(artifactPath), "installer-")
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
		}
	}()
	helper := filepath.Join(dir, filepath.Base(helperPath(executable)))
	if err := copyFile(helperPath(executable), helper); err != nil {
		return fmt.Errorf("stage updater: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(dir, "install.json")
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		return err
	}
	cmd := exec.Command(helper, "--manifest", manifestPath)
	detach(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer func() { _ = stdout.Close() }()
	if err := cmd.Start(); err != nil {
		return err
	}
	ready := make(chan error, 1)
	go func() {
		var result response
		if err := json.NewDecoder(io.LimitReader(stdout, 8192)).Decode(&result); err != nil {
			ready <- fmt.Errorf("updater did not become ready: %w", err)
		} else if !result.Ready {
			ready <- errors.New(result.Error)
		} else {
			ready <- nil
		}
	}()
	select {
	case err = <-ready:
	case <-time.After(5 * time.Minute):
		// Let the helper unmount and clean up normally when preparation finishes.
		// Killing it here could leave a mounted disk image behind.
		_ = os.WriteFile(filepath.Join(dir, "cancel"), nil, 0600)
		success = true
		_ = cmd.Process.Release()
		return errors.New("updater preparation timed out")
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	success = true
	return cmd.Process.Release()
}

func readManifest(path string) (manifest, error) {
	var m manifest
	if err := regularFile(path); err != nil {
		return m, err
	}
	file, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 65536))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, err
	}
	return m, m.validate()
}

func (m manifest) validate() error {
	if !m.Messages.Valid() {
		return errors.New("native update messages are missing or invalid")
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(m.Version) || m.PID <= 1 {
		return errors.New("invalid update version or parent process")
	}
	for _, path := range []string{m.Artifact, m.Executable} {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
			return errors.New("update paths must be absolute")
		}
		if err := regularFile(path); err != nil {
			return err
		}
	}
	return validateTarget(m.Executable)
}

func installationDigest(executable string) (string, error) {
	paths := []string{executable}
	if runtime.GOOS == "windows" {
		// The Go backend contains the version; the Qt executable may be identical between releases.
		paths = append(paths, filepath.Join(filepath.Dir(executable), "libserver.dll"))
	}
	hash := sha256.New()
	for _, path := range paths {
		if err := regularFile(path); err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(hash, file)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func (m manifest) unchanged() error {
	digest, err := installationDigest(m.Executable)
	if err != nil {
		return err
	}
	if m.CurrentDigest == "" || digest != m.CurrentDigest {
		return errors.New("the installed app changed while preparing the update; please restart it and check for updates again")
	}
	return nil
}

func regularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	samePath := filepath.Clean(resolved) == filepath.Clean(path)
	if runtime.GOOS == "windows" {
		samePath = strings.EqualFold(filepath.Clean(resolved), filepath.Clean(path))
	}
	if !samePath {
		return fmt.Errorf("update paths must not contain symlinks: %s", path)
	}
	return nil
}

func copyFile(source, target string) error {
	if err := regularFile(source); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}

// Run executes the staged installer. It must run in a separate process.
func Run(path string, output io.Writer) error {
	if err := closeInheritedDescriptors(); err != nil {
		return errors.Join(err, json.NewEncoder(output).Encode(response{Error: err.Error()}))
	}
	ignoreBrokenPipe()
	m, err := readManifest(path)
	if err != nil {
		return errors.Join(err, json.NewEncoder(output).Encode(response{Error: err.Error()}))
	}
	wait, closeWait, err := watchParent(m.PID)
	if err != nil {
		return errors.Join(err, json.NewEncoder(output).Encode(response{Error: err.Error()}))
	}
	defer closeWait()
	apply, cleanup, err := prepare(m, filepath.Dir(path))
	if err != nil {
		return errors.Join(err, json.NewEncoder(output).Encode(response{Error: err.Error()}))
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "cancel")); err == nil {
		return errors.New("update installation was canceled")
	}
	if err := json.NewEncoder(output).Encode(response{Ready: true}); err != nil {
		return err
	}
	if err := wait(); err != nil {
		return err
	}
	installErr := m.unchanged()
	if installErr == nil {
		installErr = apply()
	}
	if installErr != nil {
		showError(m.Messages.InstallFailed + "\n\n" + installErr.Error())
	}
	// The original process has exited; a relaunched app has no pending-download state to reuse.
	_ = os.Remove(m.Artifact)
	launchErr := relaunch(m)
	if launchErr != nil {
		showError(m.Messages.RelaunchFailed + "\n\n" + launchErr.Error())
	}
	return errors.Join(installErr, launchErr)
}

func relaunch(m manifest) error {
	cmd := exec.Command(m.Executable, m.Arguments...)
	cmd.Dir = filepath.Dir(m.Executable)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
