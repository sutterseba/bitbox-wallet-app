// SPDX-License-Identifier: Apache-2.0

package install

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"os"
	"testing"
)

// TestInstallerPublisher rejects a matching display name with a different legal
// identity, while allowing the authenticated publisher to renew its certificate.
func TestInstallerPublisher(t *testing.T) {
	for _, test := range []struct {
		name    string
		subject pkix.Name
		want    bool
	}{
		{"publisher", pkix.Name{Organization: []string{"Shift Crypto AG"}, Country: []string{"CH"}, SerialNumber: "CHE-498.818.196"}, true},
		{"display name only", pkix.Name{CommonName: "Shift Crypto AG"}, false},
		{"different company", pkix.Name{Organization: []string{"Shift Crypto AG"}, Country: []string{"CH"}, SerialNumber: "CHE-000.000.000"}, false},
		{"different country", pkix.Name{Organization: []string{"Shift Crypto AG"}, Country: []string{"US"}, SerialNumber: "CHE-498.818.196"}, false},
		{"different publisher", pkix.Name{Organization: []string{"Another Company"}, Country: []string{"CH"}, SerialNumber: "CHE-498.818.196"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isInstallerPublisher(&x509.Certificate{Subject: test.subject}); got != test.want {
				t.Fatalf("publisher match = %v, want %v", got, test.want)
			}
		})
	}
}

// TestInstallerSignatureRejectsUnsigned exercises the native trust boundary with
// the unsigned Go test executable, which is otherwise a valid PE file.
func TestInstallerSignatureRejectsUnsigned(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallerSignature(path); err == nil {
		t.Fatal("accepted an unsigned installer")
	}
}
