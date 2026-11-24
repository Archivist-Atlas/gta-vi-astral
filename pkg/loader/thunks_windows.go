//go:build windows

package loader

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
)

const stubThunkSize = 32

// ImportSite décrit un import non encore implémenté (stocké côté Go, indexé par entier GC-safe).
type ImportSite struct {
	DLL    string
	Symbol string
	Policy core.UnimplementedPolicy
	Calls  atomic.Uint64
}

var (
	sitesMu            sync.RWMutex
	importSites        []*ImportSite
	unimplCallbackOnce sync.Once
	unimplCallbackAddr uintptr
	crashDataDir       string
)

// SetCrashDataDir configure le dossier où crashes.log est écrit en cas de trap/crash.
func SetCrashDataDir(dir string) {
	crashDataDir = dir
}

func getUnimplCallbackAddr() uintptr {
	unimplCallbackOnce.Do(func() {
		unimplCallbackAddr = syscall.NewCallback(unimplementedDispatch)
	})
	return unimplCallbackAddr
}

func unimplementedDispatch(siteID uintptr) uintptr {
	sitesMu.RLock()
	var site *ImportSite
	if int(siteID) >= 0 && int(siteID) < len(importSites) {
		site = importSites[siteID]
	}
	sitesMu.RUnlock()

	if site == nil {
		return 0
	}
	prev := site.Calls.Add(1) - 1

	switch site.Policy {
	case core.UnimplZero:
		return 0
	case core.UnimplFail:
		if prev == 0 {
			slog.Warn("import non implémenté appelé (E_NOTIMPL)", "dll", site.DLL, "symbol", site.Symbol)
		}
		return 0x80004001 // E_NOTIMPL
	case core.UnimplTrap:
		msg := fmt.Sprintf("ASTRAL: import non implémenté appelé : %s!%s", site.DLL, site.Symbol)
		slog.Error(msg)
		if crashDataDir != "" {
			_ = AppendCrashLog(crashDataDir, msg)
		}
		os.Exit(3)
		return 0
	default: // UnimplLog
		if prev == 0 {
			slog.Warn("import non implémenté appelé (retourne 0)", "dll", site.DLL, "symbol", site.Symbol)
		}
		return 0
	}
}

// ThunkBlock alloue une plage mémoire exécutable contenant les stubs x86-64 de 32 octets.
type ThunkBlock struct {
	base uintptr
	size uintptr
	used uintptr
}

// NewThunkBlock réserve de la place pour count stubs x86-64.
func NewThunkBlock(count int) (*ThunkBlock, error) {
	if count < 1 {
		count = 1
	}
	size := uintptr(count * stubThunkSize)
	size = (size + 4095) &^ 4095
	base, err := virtualAlloc(0, size, memCommit|memReserve, pageReadWrite)
	if err != nil {
		return nil, fmt.Errorf("VirtualAlloc(ThunkBlock) : %w", err)
	}
	return &ThunkBlock{base: base, size: size}, nil
}

// Emit génère un stub x86-64 :
//
//	mov rcx, <siteID>           ; 48 B9 imm64
//	mov rax, <unimplCallback>   ; 48 B8 imm64
//	jmp rax                     ; FF E0
func (b *ThunkBlock) Emit(dll, symbol string, policy core.UnimplementedPolicy) (uintptr, error) {
	if b.used+stubThunkSize > b.size {
		return 0, fmt.Errorf("bloc de thunks plein")
	}
	sitesMu.Lock()
	siteID := uintptr(len(importSites))
	importSites = append(importSites, &ImportSite{
		DLL:    dll,
		Symbol: symbol,
		Policy: policy,
	})
	sitesMu.Unlock()

	at := b.base + b.used
	slot := unsafe.Slice((*byte)(ptrFromAddr(at)), stubThunkSize)
	for i := range slot {
		slot[i] = 0xCC // int3 padding
	}
	slot[0], slot[1] = 0x48, 0xB9
	binary.LittleEndian.PutUint64(slot[2:10], uint64(siteID))
	slot[10], slot[11] = 0x48, 0xB8
	binary.LittleEndian.PutUint64(slot[12:20], uint64(getUnimplCallbackAddr()))
	slot[20], slot[21] = 0xFF, 0xE0

	b.used += stubThunkSize
	return at, nil
}

// Seal passe le bloc en lecture+exécution (PAGE_EXECUTE_READ) et vide le cache d'instructions.
func (b *ThunkBlock) Seal() error {
	if _, err := virtualProtect(b.base, b.size, pageExecuteRead); err != nil {
		return fmt.Errorf("VirtualProtect(ThunkBlock) : %w", err)
	}
	flushInstructionCache(b.base, b.size)
	return nil
}
