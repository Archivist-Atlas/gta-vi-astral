//go:build windows

package hle

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

type gameSaveState struct {
	rootDir    string
	mu         sync.Mutex
	nextHandle atomic.Uint64
	containers map[uintptr]string
	updates    map[uintptr]string
}

func sanitizeSaveName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "..", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, ":", "_")
	if name == "" {
		return "default"
	}
	return name
}

func readSaveCStr(ptr uintptr) string {
	if ptr < 0x10000 {
		return ""
	}
	b := unsafe.Slice((*byte)(osPtr(ptr)), 256)
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// newXGameSaveProvider crée l'objet COM xgamesave ({704C3F58-E629-4CC2-B197-30511B996FE2})
// adossé au répertoire overlay hors dépôt (<overlay>/saves).
func newXGameSaveProvider(vfs *VirtualFS) uintptr {
	saveRoot := filepath.Join(os.TempDir(), "astral-saves")
	if vfs != nil && vfs.OverlayDir != "" {
		saveRoot = filepath.Join(vfs.OverlayDir, "saves")
	}
	_ = os.MkdirAll(saveRoot, 0o755)

	gs := &gameSaveState{
		rootDir:    saveRoot,
		containers: make(map[uintptr]string),
		updates:    make(map[uintptr]string),
	}
	gs.nextHandle.Store(0x00005A7E00000001)

	cbOK := syscall.NewCallback(func() uintptr { return hresultOK })
	completeAsync := func(asyncBlock uintptr) {
		if asyncBlock >= 0x10000 {
			cb := *(*uintptr)(osPtr(asyncBlock + 0x10))
			if cb >= 0x10000 && cb < 0x800000000000 {
				_, _, _ = syscall.SyscallN(cb, asyncBlock)
			}
		}
	}

	return NewCOMObject("xgamesave", 40, map[int]uintptr{
		3: syscall.NewCallback(func(this, user, scid, syncOnDemand, outProv uintptr) uintptr { // XGameSaveInitializeProvider
			slog.Info("xgamesave!XGameSaveInitializeProvider -> stockage disque hors-ligne", "dir", gs.rootDir)
			if outProv >= 0x10000 {
				*(*uintptr)(osPtr(outProv)) = uintptr(gs.nextHandle.Add(1))
			}
			return hresultOK
		}),
		4: syscall.NewCallback(func(this, user, scid, syncOnDemand, asyncBlock uintptr) uintptr { // XGameSaveInitializeProviderAsync
			slog.Info("xgamesave!XGameSaveInitializeProviderAsync -> stockage disque hors-ligne", "dir", gs.rootDir)
			completeAsync(asyncBlock)
			return hresultOK
		}),
		5: syscall.NewCallback(func(this, asyncBlock, outProv uintptr) uintptr { // XGameSaveInitializeProviderResult
			if outProv >= 0x10000 {
				*(*uintptr)(osPtr(outProv)) = uintptr(gs.nextHandle.Add(1))
			}
			return hresultOK
		}),
		6: cbOK, // XGameSaveCloseProvider
		7: syscall.NewCallback(func(this, prov, outQuota uintptr) uintptr { // XGameSaveGetRemainingQuota
			if outQuota >= 0x10000 {
				*(*int64)(osPtr(outQuota)) = 256 << 20 // 256 MiB
			}
			return hresultOK
		}),
		8: syscall.NewCallback(func(this, prov, asyncBlock uintptr) uintptr { // XGameSaveGetRemainingQuotaAsync
			completeAsync(asyncBlock)
			return hresultOK
		}),
		9: syscall.NewCallback(func(this, asyncBlock, outQuota uintptr) uintptr { // XGameSaveGetRemainingQuotaResult
			if outQuota >= 0x10000 {
				*(*int64)(osPtr(outQuota)) = 256 << 20
			}
			return hresultOK
		}),
		10: cbOK, // XGameSaveEnumerateContainerInfo
		11: cbOK, // XGameSaveEnumerateContainerInfoByName
		12: cbOK, // XGameSaveGetContainerInfo
		13: cbOK, // XGameSaveDeleteContainer
		14: syscall.NewCallback(func(this, prov, namePtr, outCont uintptr) uintptr { // XGameSaveCreateContainer
			name := sanitizeSaveName(readSaveCStr(namePtr))
			dir := filepath.Join(gs.rootDir, name)
			_ = os.MkdirAll(dir, 0o755)
			h := uintptr(gs.nextHandle.Add(1))
			gs.mu.Lock()
			gs.containers[h] = dir
			gs.mu.Unlock()
			if outCont >= 0x10000 {
				*(*uintptr)(osPtr(outCont)) = h
			}
			return hresultOK
		}),
		15: syscall.NewCallback(func(this, cont uintptr) uintptr { // XGameSaveCloseContainer
			gs.mu.Lock()
			delete(gs.containers, cont)
			gs.mu.Unlock()
			return hresultOK
		}),
		16: cbOK, // XGameSaveEnumerateBlobInfo
		17: cbOK, // XGameSaveEnumerateBlobInfoByName
		18: cbOK, // XGameSaveReadBlobData
		19: syscall.NewCallback(func(this, cont, displayNamePtr, outUpd uintptr) uintptr { // XGameSaveCreateUpdate
			gs.mu.Lock()
			dir := gs.containers[cont]
			if dir == "" {
				dir = filepath.Join(gs.rootDir, "default")
				_ = os.MkdirAll(dir, 0o755)
			}
			h := uintptr(gs.nextHandle.Add(1))
			gs.updates[h] = dir
			gs.mu.Unlock()
			if outUpd >= 0x10000 {
				*(*uintptr)(osPtr(outUpd)) = h
			}
			return hresultOK
		}),
		20: syscall.NewCallback(func(this, upd uintptr) uintptr { // XGameSaveCloseUpdate
			gs.mu.Lock()
			delete(gs.updates, upd)
			gs.mu.Unlock()
			return hresultOK
		}),
		21: syscall.NewCallback(func(this, upd, blobNamePtr, dataPtr, byteCount uintptr) uintptr { // XGameSaveSubmitBlobWrite
			gs.mu.Lock()
			dir := gs.updates[upd]
			gs.mu.Unlock()
			if dir != "" && dataPtr >= 0x10000 && byteCount > 0 && byteCount <= (16<<20) {
				bname := sanitizeSaveName(readSaveCStr(blobNamePtr))
				data := unsafe.Slice((*byte)(osPtr(dataPtr)), byteCount)
				_ = os.WriteFile(filepath.Join(dir, bname+".bin"), data, 0o644)
			}
			return hresultOK
		}),
		22: cbOK, // XGameSaveSubmitBlobDelete
		23: cbOK, // XGameSaveSubmitUpdate
		24: syscall.NewCallback(func(this, upd, asyncBlock uintptr) uintptr { // XGameSaveSubmitUpdateAsync
			completeAsync(asyncBlock)
			return hresultOK
		}),
		25: cbOK, // XGameSaveSubmitUpdateResult
	})
}
