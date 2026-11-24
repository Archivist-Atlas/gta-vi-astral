//go:build !windows

package loader

import (
	"fmt"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/hle"
)

type LiveSession struct {
	BoundHLE      int
	BoundHost     int
	BoundTitleDLL int
	BoundStubs    int
	TLSSlot       uint32
	TLSPatched    int
}

type MapOptions struct {
	ExePath       string
	CacheDir      string
	DataDir       string
	Unimplemented core.UnimplementedPolicy
	LiveHLE       *hle.LiveTable
	HostPolicy    hle.HostPolicy
}

func MapAndBind(_ MapOptions) (*LiveSession, error) {
	return nil, fmt.Errorf("le chargeur PE natif requiert Windows x64")
}

func (s *LiveSession) RunEntryPoint() (uint32, error) {
	return 0, fmt.Errorf("l'exécution native requiert Windows x64")
}
