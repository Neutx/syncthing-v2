//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/single"
)

// attachConsole is needed only for the Windows GUI subsystem.
func attachConsole() {}

// confirm asks a yes/no question on the terminal. Without a terminal to ask
// on (stdin closed or not interactive and empty), the answer is no.
func confirm(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// messageBox is nil off Windows: the tray's About item opens the dashboard,
// whose Settings view has the About section.
func messageBox() func(title, text string) { return nil }

// userName is the login name of the current user.
func userName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if n := os.Getenv("USER"); n != "" {
		return n
	}
	return "this user"
}

// offerInstall is Windows-only: macOS and Linux install through the .app,
// the .deb or `stv2 install`.
func offerInstall(*single.Lock) (bool, int) { return false, exitOK }
