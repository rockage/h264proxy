// Command genkey writes a fresh random 32-byte AES-256 key to a file. The key
// is never printed; only its fingerprint is shown.
package main

import (
	"flag"
	"fmt"
	"os"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
)

func main() {
	out := flag.String("out", "testdata/key.bin", "path to write the 32-byte key file")
	force := flag.Bool("force", false, "overwrite an existing key file")
	flag.Parse()

	if _, err := os.Stat(*out); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "refusing to overwrite existing key %q (use -force)\n", *out)
		os.Exit(1)
	}

	key, err := cryptox.GenerateKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate key:", err)
		os.Exit(1)
	}
	// 0600: the key is a secret.
	if err := os.WriteFile(*out, key, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write key:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d-byte key to %s (fingerprint %s)\n", len(key), *out, cryptox.KeyFingerprint(key))
}
