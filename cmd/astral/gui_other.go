//go:build !windows

package main

func attachParentConsoleIfCLI() {}

func openGameWindowEarly() {}

func runDefaultUI() {
	runInteractiveMenu()
}
