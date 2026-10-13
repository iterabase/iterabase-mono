// Command password-hash reads one password from stdin and prints the control
// plane's Argon2id encoding, for preview tooling that seeds a known Admin
// (HOR-582). The password never appears in arguments or output.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/passwordhash"
)

func main() {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "password-hash: read password from stdin")
		os.Exit(1)
	}
	password := strings.TrimRight(line, "\r\n")
	if len([]rune(password)) < 12 || len([]rune(password)) > 128 {
		fmt.Fprintln(os.Stderr, "password-hash: the password must be 12-128 characters")
		os.Exit(1)
	}
	encoded, err := passwordhash.Hash(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "password-hash:", err)
		os.Exit(1)
	}
	fmt.Println(encoded)
}
