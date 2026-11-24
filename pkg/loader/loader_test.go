package loader

import (
	"testing"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/hle"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/pe"
)

func TestImportResolutionOrder(t *testing.T) {
	res := NewResolver(hle.NewDefaultRegistry(), hle.DefaultHostPolicy(), "")

	if k, _ := res.Resolve("xgameruntime.dll", "XUserAddAsync"); k != ResolveHLE {
		t.Fatalf("xgameruntime.dll doit résoudre en HLE, obtenu %s", k)
	}
	if k, _ := res.Resolve("d3d12_xs.dll", "D3D12XboxCreateDevice"); k != ResolveHLE {
		t.Fatalf("d3d12_xs.dll doit résoudre en HLE, obtenu %s", k)
	}
	if k, _ := res.Resolve("KERNEL32.dll", "CreateThread"); k != ResolveHost {
		t.Fatalf("kernel32.dll doit résoudre en Host, obtenu %s", k)
	}
	if k, _ := res.Resolve("ws2_32.dll", "connect"); k != ResolveHLE {
		t.Fatalf("ws2_32.dll!connect doit être intercepté en HLE hors-ligne, obtenu %s", k)
	}
	if k, _ := res.Resolve("unknown_console_only.dll", "FooBar"); k != ResolveUnimplemented {
		t.Fatalf("DLL inconnue doit résoudre en Unimplemented, obtenu %s", k)
	}

	info := &pe.ImageInfo{
		Imports: []pe.ImportModule{
			{DLL: "xgameruntime.dll", Entries: []pe.ImportEntry{{Name: "QueryApiImpl"}}},
			{DLL: "kernel32.dll", Entries: []pe.ImportEntry{{Name: "VirtualAlloc"}}},
			{DLL: "mystery.dll", Entries: []pe.ImportEntry{{Name: "MissingFn"}}},
		},
	}
	rep := res.Census("test.exe", info)
	if rep.Total != 3 || rep.ByKind[ResolveHLE] != 1 || rep.ByKind[ResolveHost] != 1 || rep.ByKind[ResolveUnimplemented] != 1 {
		t.Fatalf("census inattendu : %+v", rep.ByKind)
	}
}
