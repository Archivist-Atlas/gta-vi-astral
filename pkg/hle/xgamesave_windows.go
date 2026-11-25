//go:build windows

package hle

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/gpu"
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

	gdkTrace := os.Getenv("ASTRAL_GDK_TRACE") == "1"
	cbOK := syscall.NewCallback(func() uintptr { return hresultOK })
	traceSlot := func(name string, fn uintptr) uintptr {
		if !gdkTrace {
			return fn
		}
		return syscall.NewCallback(func(a0, a1, a2, a3, a4, a5 uintptr) uintptr {
			slog.Info("GDK_TRACE xgamesave", "op", name, "a1", fmt.Sprintf("0x%X", a1), "a2", fmt.Sprintf("0x%X", a2))
			r, _, _ := syscall.SyscallN(fn, a0, a1, a2, a3, a4, a5)
			return r
		})
	}
	_ = traceSlot
	completeAsync := func(asyncBlock uintptr) {
		if asyncBlock >= 0x10000 {
			cb := *(*uintptr)(osPtr(asyncBlock + 0x10))
			if cb >= 0x10000 && cb < 0x800000000000 {
				go func() {
					time.Sleep(5 * time.Millisecond)
					_, _, _ = syscall.SyscallN(cb, asyncBlock)
				}()
			}
		}
	}

	return NewCOMObject("xgamesave", 32, map[int]uintptr{
		3: syscall.NewCallback(func(this, user, scid, syncOnDemand, outProv uintptr) uintptr { // 0x18 XGameSaveInitializeProvider
			slog.Info("xgamesave!XGameSaveInitializeProvider -> stockage disque hors-ligne", "dir", gs.rootDir)
			gpu.ReportBootProgress(0.90)
			if outProv >= 0x10000 {
				*(*uintptr)(osPtr(outProv)) = uintptr(gs.nextHandle.Add(1))
			}
			return hresultOK
		}),
		4: syscall.NewCallback(func(this, user, scid, syncOnDemand, asyncBlock uintptr) uintptr { // 0x20 XGameSaveInitializeProviderAsync
			slog.Info("xgamesave!XGameSaveInitializeProviderAsync -> stockage disque hors-ligne", "dir", gs.rootDir)
			gpu.ReportBootProgress(0.90)
			completeAsync(asyncBlock)
			return hresultOK
		}),
		5: traceSlot("InitializeProviderResult", syscall.NewCallback(func(this, asyncBlock, outProv uintptr) uintptr { // 0x28 XGameSaveInitializeProviderResult
			if outProv >= 0x10000 {
				*(*uintptr)(osPtr(outProv)) = uintptr(gs.nextHandle.Add(1))
			}
			return hresultOK
		})),
		6: traceSlot("CloseProvider", cbOK), // 0x30 XGameSaveCloseProvider
		7: traceSlot("GetRemainingQuota", syscall.NewCallback(func(this, prov, outQuota uintptr) uintptr { // 0x38 XGameSaveGetRemainingQuota
			if outQuota >= 0x10000 {
				*(*int64)(osPtr(outQuota)) = 256 << 20 // 256 MiB
			}
			return hresultOK
		})),
		8: traceSlot("GetRemainingQuotaAsync", syscall.NewCallback(func(this, prov, asyncBlock uintptr) uintptr { // 0x40 XGameSaveGetRemainingQuotaAsync
			completeAsync(asyncBlock)
			return hresultOK
		})),
		9: traceSlot("GetRemainingQuotaResult", syscall.NewCallback(func(this, asyncBlock, outQuota uintptr) uintptr { // 0x48 XGameSaveGetRemainingQuotaResult
			if outQuota >= 0x10000 {
				*(*int64)(osPtr(outQuota)) = 256 << 20
			}
			return hresultOK
		})),
		10: traceSlot("DeleteContainer", cbOK), // 0x50 XGameSaveDeleteContainer
		11: traceSlot("DeleteContainerAsync", syscall.NewCallback(func(this, prov, namePtr, asyncBlock uintptr) uintptr { // 0x58 XGameSaveDeleteContainerAsync
			completeAsync(asyncBlock)
			return hresultOK
		})),
		12: traceSlot("DeleteContainerResult", cbOK), // 0x60 XGameSaveDeleteContainerResult
		13: traceSlot("GetContainerInfo", syscall.NewCallback(func(this, prov, namePtr, ctx, cb uintptr) uintptr { // 0x68 XGameSaveGetContainerInfo
			return hresultOK // Aucun conteneur existant -> S_OK sans invoquer callback (ABI GDK)
		})),
		14: traceSlot("EnumerateContainerInfo", cbOK),       // 0x70 XGameSaveEnumerateContainerInfo
		15: traceSlot("EnumerateContainerInfoByName", cbOK), // 0x78 XGameSaveEnumerateContainerInfoByName
		16: traceSlot("CreateContainer", syscall.NewCallback(func(this, prov, namePtr, outCont uintptr) uintptr { // 0x80 XGameSaveCreateContainer
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
		})),
		17: traceSlot("CloseContainer", syscall.NewCallback(func(this, cont uintptr) uintptr { // 0x88 XGameSaveCloseContainer
			gs.mu.Lock()
			delete(gs.containers, cont)
			gs.mu.Unlock()
			return hresultOK
		})),
		18: traceSlot("EnumerateBlobInfo", cbOK),       // 0x90 XGameSaveEnumerateBlobInfo
		19: traceSlot("EnumerateBlobInfoByName", cbOK), // 0x98 XGameSaveEnumerateBlobInfoByName
		20: traceSlot("ReadBlobData", cbOK),            // 0xA0 XGameSaveReadBlobData
		21: traceSlot("ReadBlobDataAsync", syscall.NewCallback(func(this, cont, blobNames, count, asyncBlock uintptr) uintptr { // 0xA8 XGameSaveReadBlobDataAsync
			completeAsync(asyncBlock)
			return hresultOK
		})),
		22: traceSlot("ReadBlobDataResult", syscall.NewCallback(func(this, asyncBlock, blobsSize, blobsBuf, outCount uintptr) uintptr { // 0xB0 XGameSaveReadBlobDataResult
			if outCount >= 0x10000 {
				*(*uint32)(osPtr(outCount)) = 0
			}
			return hresultOK
		})),
		23: traceSlot("CreateUpdate", syscall.NewCallback(func(this, cont, displayNamePtr, outUpd uintptr) uintptr { // 0xB8 XGameSaveCreateUpdate
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
		})),
		24: traceSlot("CloseUpdate", syscall.NewCallback(func(this, upd uintptr) uintptr { // 0xC0 XGameSaveCloseUpdate
			gs.mu.Lock()
			delete(gs.updates, upd)
			gs.mu.Unlock()
			return hresultOK
		})),
		25: traceSlot("SubmitBlobWrite", syscall.NewCallback(func(this, upd, blobNamePtr, dataPtr, byteCount uintptr) uintptr { // 0xC8 XGameSaveSubmitBlobWrite
			gs.mu.Lock()
			dir := gs.updates[upd]
			gs.mu.Unlock()
			if dir != "" && dataPtr >= 0x10000 && byteCount > 0 && byteCount <= (16<<20) {
				bname := sanitizeSaveName(readSaveCStr(blobNamePtr))
				data := unsafe.Slice((*byte)(osPtr(dataPtr)), byteCount)
				_ = os.WriteFile(filepath.Join(dir, bname+".bin"), data, 0o644)
			}
			return hresultOK
		})),
		26: traceSlot("SubmitBlobDelete", cbOK), // 0xD0 XGameSaveSubmitBlobDelete
		27: traceSlot("SubmitUpdate", cbOK),     // 0xD8 XGameSaveSubmitUpdate
		28: traceSlot("SubmitUpdateAsync", syscall.NewCallback(func(this, upd, asyncBlock uintptr) uintptr { // 0xE0 XGameSaveSubmitUpdateAsync
			completeAsync(asyncBlock)
			return hresultOK
		})),
		29: traceSlot("SubmitUpdateResult", cbOK), // 0xE8 XGameSaveSubmitUpdateResult
	})
}
