// SPDX-License-Identifier: Apache-2.0

package appupdate

import "strings"

// InstallMessages carries translated UI text to the helper, which outlives the frontend.
type InstallMessages struct {
	InstallFailed  string `json:"installFailed"`
	RelaunchFailed string `json:"relaunchFailed"`
}

// Valid bounds the text accepted from the UI and prevents empty native error dialogs.
func (m InstallMessages) Valid() bool {
	for _, value := range []string{m.InstallFailed, m.RelaunchFailed} {
		if strings.TrimSpace(value) == "" || len(value) > 4096 || strings.ContainsRune(value, '\x00') {
			return false
		}
	}
	return true
}
