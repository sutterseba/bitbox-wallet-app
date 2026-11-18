// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !windows

package install

import (
	"errors"
	"os/exec"
)

func ignoreBrokenPipe() {}

func closeInheritedDescriptors() error { return nil }

func unsupported() error                                     { return errors.New("native app updates are unavailable on this platform") }
func detach(*exec.Cmd)                                       {}
func validateTarget(string) error                            { return unsupported() }
func prepare(manifest, string) (func() error, func(), error) { return nil, nil, unsupported() }
func watchParent(int) (func() error, func(), error)          { return nil, nil, unsupported() }
func showError(string)                                       {}

// ApplyMacOS is unavailable on this platform.
func ApplyMacOS(string) error { return unsupported() }
