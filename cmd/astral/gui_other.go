//go:build !windows

package main

func attachParentConsoleIfCLI() {}

func openGameWindowEarly(bool) {}

func runDefaultUI() {
	runInteractiveMenu()
}
