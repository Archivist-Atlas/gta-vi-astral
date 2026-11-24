//go:build !windows

package hle

import "github.com/Archivist-Atlas/gta-vi-astral/pkg/core"

type LiveTable struct{}

func NewLiveTable(_ core.ConsoleProfile) *LiveTable { return &LiveTable{} }
func (*LiveTable) Lookup(_, _ string) (uintptr, bool) { return 0, false }
