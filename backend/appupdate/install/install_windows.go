// SPDX-License-Identifier: Apache-2.0

package install

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ignoreBrokenPipe() {}

func closeInheritedDescriptors() error { return nil }

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow: true}
}

func validateTarget(executable string) error {
	if !strings.EqualFold(filepath.Base(executable), "BitBox.exe") {
		return errors.New("updates require an installed BitBox.exe")
	}
	for _, path := range []string{executable, filepath.Join(filepath.Dir(executable), "libserver.dll"), filepath.Join(filepath.Dir(executable), "assets.rcc")} {
		if err := regularFile(path); err != nil {
			return err
		}
	}
	name, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return err
	}
	volume := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(name, &volume[0], uint32(len(volume))); err != nil {
		return err
	}
	var flags uint32
	if err := windows.GetVolumeInformation(&volume[0], nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return err
	}
	if flags&windows.FILE_READ_ONLY_VOLUME != 0 {
		return errors.New("move BitBoxApp to a writable volume before updating")
	}
	return nil
}

func validateInstaller(path, version string) error {
	if err := verifyInstallerSignature(path); err != nil {
		return err
	}
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return err
	}
	if size == 0 || size > 1024*1024 {
		return errors.New("installer version resource is missing or too large")
	}
	data := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&data[0])); err != nil {
		return err
	}
	var value unsafe.Pointer
	var length uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&data[0]), `\`, unsafe.Pointer(&value), &length); err != nil {
		return err
	}
	// VS_FIXEDFILEINFO contains the version independently of localized string resources.
	if length < 52 {
		return errors.New("invalid installer version information")
	}
	fields := unsafe.Slice((*uint32)(value), 13)
	parts := strings.Split(version, ".")
	expected := make([]uint64, 3)
	for i, part := range parts {
		number, err := strconv.ParseUint(part, 10, 16)
		if err != nil {
			return err
		}
		expected[i] = number
	}
	if fields[0] != 0xfeef04bd || fields[4] != uint32(expected[0]<<16|expected[1]) || fields[5] != uint32(expected[2]<<16) {
		return errors.New("installer version does not match the selected release")
	}
	if err := windows.VerQueryValue(unsafe.Pointer(&data[0]), `\VarFileInfo\Translation`, unsafe.Pointer(&value), &length); err != nil {
		return err
	}
	if length < 4 {
		return errors.New("installer product information is missing")
	}
	translation := unsafe.Slice((*uint16)(value), 2)
	product := fmt.Sprintf(`\StringFileInfo\%04x%04x\ProductName`, translation[0], translation[1])
	if err := windows.VerQueryValue(unsafe.Pointer(&data[0]), product, unsafe.Pointer(&value), &length); err != nil {
		return err
	}
	if length == 0 || length > 1024 || windows.UTF16ToString(unsafe.Slice((*uint16)(value), length)) != "BitBoxApp" {
		return errors.New("installer is not a BitBoxApp release")
	}
	return nil
}

// verifyInstallerSignature verifies the Authenticode signature before trusting its
// signer. The publisher's legal identity survives code-signing certificate renewal.
func verifyInstallerSignature(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	file := windows.WinTrustFileInfo{FilePath: name}
	file.Size = uint32(unsafe.Sizeof(file))
	data := windows.WinTrustData{
		UIChoice: windows.WTD_UI_NONE, UnionChoice: windows.WTD_CHOICE_FILE,
		FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(&file),
		StateAction:                     windows.WTD_STATEACTION_VERIFY,
		// Verify offline so Windows does not bypass the app's configured proxy or require cached OCSP.
		ProvFlags: windows.WTD_CACHE_ONLY_URL_RETRIEVAL | windows.WTD_REVOCATION_CHECK_NONE,
	}
	data.Size = uint32(unsafe.Sizeof(data))
	defer func() {
		data.StateAction = windows.WTD_STATEACTION_CLOSE
		_ = windows.WinVerifyTrustEx(windows.HWND(windows.InvalidHandle), &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, &data)
	}()
	if err := windows.WinVerifyTrustEx(windows.HWND(windows.InvalidHandle), &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, &data); err != nil {
		return fmt.Errorf("verify installer signature: %w", err)
	}

	trust := windows.NewLazySystemDLL("wintrust.dll")
	provider, _, _ := trust.NewProc("WTHelperProvDataFromStateData").Call(uintptr(data.StateData))
	if provider == 0 {
		return errors.New("installer signature has no verified provider")
	}
	signer, _, _ := trust.NewProc("WTHelperGetProvSignerFromChain").Call(provider, 0, 0, 0)
	if signer == 0 {
		return errors.New("installer signature has no verified signer")
	}
	certificate, _, _ := trust.NewProc("WTHelperGetProvCertFromChain").Call(signer, 0)
	if certificate == 0 {
		return errors.New("installer signature has no verified certificate")
	}
	// Copy the documented CRYPT_PROVIDER_CERT prefix while Wintrust owns its memory.
	var verified struct {
		Size uint32
		Cert *windows.CertContext
	}
	if err := windows.ReadProcessMemory(windows.CurrentProcess(), certificate,
		(*byte)(unsafe.Pointer(&verified)), unsafe.Sizeof(verified), nil); err != nil {
		return fmt.Errorf("read verified signer certificate: %w", err)
	}
	if verified.Cert == nil {
		return errors.New("installer signer certificate is missing")
	}
	cert, err := x509.ParseCertificate(unsafe.Slice(verified.Cert.EncodedCert, verified.Cert.Length))
	if err != nil {
		return fmt.Errorf("read installer signer: %w", err)
	}
	if !isInstallerPublisher(cert) {
		return errors.New("installer is not signed by Shift Crypto AG")
	}
	return nil
}

func isInstallerPublisher(cert *x509.Certificate) bool {
	return len(cert.Subject.Organization) == 1 && cert.Subject.Organization[0] == "Shift Crypto AG" &&
		len(cert.Subject.Country) == 1 && cert.Subject.Country[0] == "CH" &&
		cert.Subject.SerialNumber == "CHE-498.818.196"
}

func prepare(m manifest, _ string) (func() error, func(), error) {
	if err := validateInstaller(m.Artifact, m.Version); err != nil {
		return nil, nil, err
	}
	return func() error {
			// Recheck immediately before handing executable code to the elevated installer.
			if err := validateInstaller(m.Artifact, m.Version); err != nil {
				return err
			}
			// NSIS requires /D to be last and unquoted, including directories containing spaces.
			return runInstaller(m.Artifact, "/S /D="+filepath.Dir(m.Executable))
		}, func() {
			// Windows may retain the running helper until it exits; Run removes downloaded assets.
			helper, err := os.Executable()
			if err == nil {
				_ = os.Remove(filepath.Join(filepath.Dir(helper), "install.json"))
				_ = os.Remove(helper)
				_ = os.Remove(filepath.Dir(helper))
				_ = os.Remove(filepath.Dir(filepath.Dir(helper)))
			}
		}, nil
}

type shellExecuteInfo struct {
	Size       uint32
	Mask       uint32
	Window     windows.Handle
	Verb       *uint16
	File       *uint16
	Parameters *uint16
	Directory  *uint16
	Show       int32
	Instance   windows.Handle
	IDList     uintptr
	Class      *uint16
	ClassKey   windows.Handle
	HotKey     uint32
	Icon       windows.Handle
	Process    windows.Handle
}

func runInstaller(path, parameters string) error {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	args, err := windows.UTF16PtrFromString(parameters)
	if err != nil {
		return err
	}
	info := shellExecuteInfo{Mask: 0x00000040 | 0x00000100, Verb: verb, File: file, Parameters: args, Show: windows.SW_SHOWNORMAL}
	info.Size = uint32(unsafe.Sizeof(info))
	result, _, callErr := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW").Call(uintptr(unsafe.Pointer(&info)))
	if result == 0 {
		return fmt.Errorf("start installer: %w", callErr)
	}
	if info.Process == 0 {
		return errors.New("installer did not return a process handle")
	}
	defer func() { _ = windows.CloseHandle(info.Process) }()
	if _, err := windows.WaitForSingleObject(info.Process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.Process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("installer exited with code %d", code)
	}
	return nil
}

func watchParent(pid int) (func() error, func(), error) {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return nil, nil, err
	}
	return func() error {
		status, err := windows.WaitForSingleObject(process, 120000)
		if err != nil {
			return err
		}
		if status != windows.WAIT_OBJECT_0 {
			return errors.New("BitBoxApp did not exit; update was not installed")
		}
		return nil
	}, func() { _ = windows.CloseHandle(process) }, nil
}

func showError(message string) {
	text, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return
	}
	title, _ := windows.UTF16PtrFromString("BitBoxApp")
	_, _, _ = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW").Call(0,
		uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}

// ApplyMacOS is unavailable on Windows.
func ApplyMacOS(string) error {
	return errors.New("macOS installation is unavailable")
}
