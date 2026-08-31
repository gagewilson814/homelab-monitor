// hashpw generates a bcrypt hash for HOMELAB_PASSWORD_HASH.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	fmt.Fprint(os.Stderr, "Password: ")
	password, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "error reading password:", err)
		os.Exit(1)
	}
	password = strings.TrimRight(password, "\r\n")

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error generating hash:", err)
		os.Exit(1)
	}

	fmt.Println(string(hash))
}
