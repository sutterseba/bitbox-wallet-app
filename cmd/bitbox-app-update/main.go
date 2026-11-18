// SPDX-License-Identifier: Apache-2.0

// The updater runs outside the installed app so its files can be replaced after shutdown.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/BitBoxSwiss/bitbox-wallet-app/backend/appupdate/install"
)

func main() {
	manifest := flag.String("manifest", "", "private installation manifest")
	apply := flag.Bool("apply-macos", false, "apply an authorized macOS bundle replacement")
	flag.Parse()
	var err error
	if *apply {
		err = install.ApplyMacOS(*manifest)
	} else {
		err = install.Run(*manifest, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
