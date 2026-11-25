//go:build !windows

package main

func attachParentConsoleIfCLI() {}

func runDefaultUI() {
	runInteractiveMenu()
}
