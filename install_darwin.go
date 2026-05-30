//go:build darwin

package main

import (
	"fmt"
	"os"
)

func runInstall() error {
	// Root privilege check
	if os.Getuid() != 0 {
		return fmt.Errorf("werunos install requires root (sudo) privileges.\nPlease run: sudo ./werunos install")
	}

	if isMacFUSEInstalled() {
		fmt.Println("macFUSE is already installed. werunos is ready to use!")
		return nil
	}

	fmt.Println("macFUSE is not installed or not detected.")
	fmt.Println("To install macFUSE on macOS, please run:")
	fmt.Println("  brew install --cask macfuse")
	fmt.Println()
	fmt.Println("Or download and install it manually from the official releases:")
	fmt.Println("  https://macfuse.github.io/")
	fmt.Println()
	fmt.Println("Note: After installation, you may need to enable the macFUSE system extension")
	fmt.Println("in System Settings -> Privacy & Security -> Security and restart your Mac.")
	return nil
}

func isMacFUSEInstalled() bool {
	// macFUSE installs its filesystem bundle at this standard path:
	_, err := os.Stat("/Library/Filesystems/macfuse.fs")
	return err == nil
}
