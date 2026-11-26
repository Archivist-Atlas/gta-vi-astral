//go:build !windows

package main

func attachParentConsoleIfCLI() {}

func openGameWindowEarly(bool, bool) {}

func runDefaultUI() {
	runInteractiveMenu()
}
