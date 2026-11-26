//go:build windows

package hle

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/gpu"
)

const (
	hresultGSQuotaExceeded  uint32 = 0x80830006
	hresultGSBufferTooSmall uint32 = 0x80830007
	hresultGSBlobNotFound   uint32 = 0x80830008
	gsMaxQuotaBytes         int64  = 256 << 20
	gsBlobStructSize        int    = 24
)

type gsContainerMeta struct {
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name"`
	UpdatedUnix int64             `json:"updated_unix"`
	Blobs       map[string]string `json:"blobs"` // exact blob name -> filename on disk
}

type gsContainerHandle struct {
	name string
	dir  string
}

type gsBlobChange struct {
	name   string
	data   []byte // nil means delete
	delete bool
}

type gsUpdateHandle struct {
	name        string
	dir         string
	displayName string
	changes     []gsBlobChange
}

type gsAsyncReadResult struct {
	hr     uint32
	packed []byte // relative offsets from 0
	count  uint32
}

type gameSaveState struct {
	rootDir     string
	mu          sync.Mutex
	nextHandle  atomic.Uint64
	containers  map[uintptr]*gsContainerHandle
	updates     map[uintptr]*gsUpdateHandle
	asyncReads  map[uintptr]*gsAsyncReadResult
	asyncStatus map[uintptr]uint32
}

func sanitizeSaveName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for i := 0; i < len(name) && b.Len() < 80; i++ {
		c := name[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	res := b.String()
	if res == "" {
		res = "default"
	}
	return res
}

func gsContainerDir(root, name string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return filepath.Join(root, fmt.Sprintf("%s~%016x", sanitizeSaveName(name), h.Sum64()))
}

func gsBlobFileName(name string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%s~%016x.bin", sanitizeSaveName(name), h.Sum64())
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

func (gs *gameSaveState) loadContainerMetaLocked(dir string) (*gsContainerMeta, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, false
	}
	var m gsContainerMeta
	if err := json.Unmarshal(raw, &m); err != nil || m.Name == "" {
		return nil, false
	}
	if m.Blobs == nil {
		m.Blobs = make(map[string]string)
	}
	return &m, true
}

func (gs *gameSaveState) containerTotalSizeLocked(dir string, m *gsContainerMeta) uint64 {
	var total uint64
	for _, fn := range m.Blobs {
		if st, err := os.Stat(filepath.Join(dir, fn)); err == nil {
			total += uint64(st.Size())
		}
	}
	return total
}

func (gs *gameSaveState) usedBytesLocked() int64 {
	entries, err := os.ReadDir(gs.rootDir)
	if err != nil {
		return 0
	}
	var total uint64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(gs.rootDir, e.Name())
		if m, ok := gs.loadContainerMetaLocked(dir); ok {
			total += gs.containerTotalSizeLocked(dir, m)
		}
	}
	return int64(total)
}

func (gs *gameSaveState) listContainersLocked(keep func(name string) bool) []struct {
	meta  gsContainerMeta
	total uint64
} {
	entries, err := os.ReadDir(gs.rootDir)
	if err != nil {
		return nil
	}
	var out []struct {
		meta  gsContainerMeta
		total uint64
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(gs.rootDir, e.Name())
		m, ok := gs.loadContainerMetaLocked(dir)
		if !ok {
			continue
		}
		if keep != nil && !keep(m.Name) {
			continue
		}
		out = append(out, struct {
			meta  gsContainerMeta
			total uint64
		}{
			meta:  *m,
			total: gs.containerTotalSizeLocked(dir, m),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].meta.Name < out[j].meta.Name })
	return out
}

func (gs *gameSaveState) applyUpdateLocked(u *gsUpdateHandle) uint32 {
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		slog.Warn("xgamesave!SubmitUpdate mkdir échoué", "container", u.name, "err", err)
		return 0x80830002 // E_GS_NO_ACCESS
	}
	meta, ok := gs.loadContainerMetaLocked(u.dir)
	if !ok {
		meta = &gsContainerMeta{
			Name:        u.name,
			DisplayName: u.displayName,
			Blobs:       make(map[string]string),
		}
	}
	if u.displayName != "" {
		meta.DisplayName = u.displayName
	}
	meta.UpdatedUnix = time.Now().Unix()

	for _, ch := range u.changes {
		if ch.delete {
			if oldFn, exists := meta.Blobs[ch.name]; exists {
				_ = os.Remove(filepath.Join(u.dir, oldFn))
				delete(meta.Blobs, ch.name)
			}
			slog.Info("xgamesave!delete-blob", "container", u.name, "blob", ch.name)
			continue
		}
		fn := gsBlobFileName(ch.name)
		dst := filepath.Join(u.dir, fn)
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, ch.data, 0o644); err != nil {
			slog.Warn("xgamesave!SubmitUpdate écriture blob échouée", "container", u.name, "blob", ch.name, "err", err)
			return 0x80830002
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(dst)
			if err2 := os.Rename(tmp, dst); err2 != nil {
				_ = os.Remove(tmp)
				return 0x80830002
			}
		}
		meta.Blobs[ch.name] = fn
		slog.Info("xgamesave!save", "container", u.name, "blob", ch.name, "bytes", len(ch.data))
	}

	rawMeta, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return 0x80830002
	}
	metaPath := filepath.Join(u.dir, "meta.json")
	metaTmp := metaPath + ".tmp"
	if err := os.WriteFile(metaTmp, rawMeta, 0o644); err != nil {
		return 0x80830002
	}
	if err := os.Rename(metaTmp, metaPath); err != nil {
		_ = os.Remove(metaPath)
		if err2 := os.Rename(metaTmp, metaPath); err2 != nil {
			_ = os.Remove(metaTmp)
			return 0x80830002
		}
	}
	return uint32(hresultOK)
}

func (gs *gameSaveState) readBlobsLocked(c *gsContainerHandle, wanted []string) ([]struct {
	name string
	data []byte
}, uint32) {
	meta, ok := gs.loadContainerMetaLocked(c.dir)
	if !ok {
		return nil, hresultGSBlobNotFound
	}
	names := wanted
	if names == nil {
		names = make([]string, 0, len(meta.Blobs))
		for n := range meta.Blobs {
			names = append(names, n)
		}
		sort.Strings(names)
	}
	out := make([]struct {
		name string
		data []byte
	}, 0, len(names))
	for _, n := range names {
		fn, exists := meta.Blobs[n]
		if !exists {
			slog.Info("xgamesave!load introuvable", "container", c.name, "blob", n)
			return nil, hresultGSBlobNotFound
		}
		data, err := os.ReadFile(filepath.Join(c.dir, fn))
		if err != nil {
			return nil, hresultGSBlobNotFound
		}
		slog.Info("xgamesave!load", "container", c.name, "blob", n, "bytes", len(data))
		out = append(out, struct {
			name string
			data []byte
		}{name: n, data: data})
	}
	return out, uint32(hresultOK)
}

// packGameSaveBlobs emballe le tableau XGameSaveBlob (24 octets/entrée : namePtr u64, size u32, pad u32, dataPtr u64)
// suivi des chaînes NUL-terminées et des données brutes, avec pointeurs relatifs à baseAddr.
func packGameSaveBlobs(blobs []struct {
	name string
	data []byte
}, baseAddr uintptr) []byte {
	total := len(blobs) * gsBlobStructSize
	for _, b := range blobs {
		total += len(b.name) + 1 + len(b.data)
	}
	out := make([]byte, total)
	cursor := len(blobs) * gsBlobStructSize
	for i, b := range blobs {
		nameOff := cursor
		copy(out[cursor:], b.name)
		cursor += len(b.name)
		out[cursor] = 0
		cursor++

		dataOff := cursor
		copy(out[cursor:], b.data)
		cursor += len(b.data)

		entry := out[i*gsBlobStructSize : (i+1)*gsBlobStructSize]
		binary.LittleEndian.PutUint64(entry[0:8], uint64(baseAddr)+uint64(nameOff))
		binary.LittleEndian.PutUint32(entry[8:12], uint32(len(b.data)))
		binary.LittleEndian.PutUint64(entry[16:24], uint64(baseAddr)+uint64(dataOff))
	}
	return out
}

// xGameSaveContainerInfoABI correspond exactement à XGameSaveContainerInfo (48 octets x64 GDK).
type xGameSaveContainerInfoABI struct {
	Name         uintptr
	DisplayName  uintptr
	BlobCount    uint32
	_pad0        uint32
	TotalSize    uint64
	LastModified int64
	NeedsSync    uint8
	_pad1        [7]byte
}

// xGameSaveBlobInfoABI correspond exactement à XGameSaveBlobInfo (16 octets x64 GDK).
type xGameSaveBlobInfoABI struct {
	Name uintptr
	Size uint32
	_pad uint32
}

func resolveSaveRoot(vfs *VirtualFS) string {
	if vfs != nil && vfs.OverlayDir != "" {
		return filepath.Join(vfs.OverlayDir, "saves")
	}
	if cfgDir, err := os.UserConfigDir(); err == nil && cfgDir != "" {
		return filepath.Join(cfgDir, "GTA VI Astral", "saves")
	}
	return filepath.Join(os.TempDir(), "astral-saves")
}

// newXGameSaveProvider crée l'objet COM xgamesave ({704C3F58-E629-4CC2-B197-30511B996FE2})
// avec persistance disque complète (réglages, profil, sauvegardes).
func newXGameSaveProvider(vfs *VirtualFS, dispatchAsync func(uintptr)) uintptr {
	saveRoot := resolveSaveRoot(vfs)
	_ = os.MkdirAll(saveRoot, 0o755)

	gs := &gameSaveState{
		rootDir:     saveRoot,
		containers:  make(map[uintptr]*gsContainerHandle),
		updates:     make(map[uintptr]*gsUpdateHandle),
		asyncReads:  make(map[uintptr]*gsAsyncReadResult),
		asyncStatus: make(map[uintptr]uint32),
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
	completeAsyncWith := func(asyncBlock uintptr, hr uint32, resultSize uint64) {
		if asyncBlock >= 0x10000 {
			atomic.StoreUint64((*uint64)(osPtr(asyncBlock+0x20)), uint64(hr))
			atomic.StoreUint64((*uint64)(osPtr(asyncBlock+0x28)), resultSize)
			if dispatchAsync != nil {
				dispatchAsync(asyncBlock)
				return
			}
			cb := *(*uintptr)(osPtr(asyncBlock + 0x10))
			if cb >= 0x10000 && cb < 0x800000000000 {
				go func() {
					time.Sleep(5 * time.Millisecond)
					_, _, _ = syscall.SyscallN(cb, asyncBlock)
				}()
			}
		}
	}
	completeAsync := func(asyncBlock uintptr) {
		completeAsyncWith(asyncBlock, uint32(hresultOK), 0)
	}

	invokeContainerCallbacks := func(keep func(string) bool, ctx, cb uintptr) uintptr {
		if cb < 0x10000 {
			return hresultInvalidArg
		}
		gs.mu.Lock()
		items := gs.listContainersLocked(keep)
		gs.mu.Unlock()
		for _, it := range items {
			nameBuf := append([]byte(it.meta.Name), 0)
			dispBuf := append([]byte(it.meta.DisplayName), 0)
			info := xGameSaveContainerInfoABI{
				Name:         uintptr(unsafe.Pointer(&nameBuf[0])),
				DisplayName:  uintptr(unsafe.Pointer(&dispBuf[0])),
				BlobCount:    uint32(len(it.meta.Blobs)),
				TotalSize:    it.total,
				LastModified: it.meta.UpdatedUnix,
				NeedsSync:    0,
			}
			cont, _, _ := syscall.SyscallN(cb, uintptr(unsafe.Pointer(&info)), ctx)
			runtime.KeepAlive(nameBuf)
			runtime.KeepAlive(dispBuf)
			runtime.KeepAlive(&info)
			if byte(cont) == 0 {
				break
			}
		}
		return hresultOK
	}

	invokeBlobCallbacks := func(cont uintptr, prefix string, ctx, cb uintptr) uintptr {
		if cb < 0x10000 {
			return hresultInvalidArg
		}
		gs.mu.Lock()
		ch := gs.containers[cont]
		type blobEntry struct {
			name string
			size uint32
		}
		var list []blobEntry
		if ch != nil {
			if m, ok := gs.loadContainerMetaLocked(ch.dir); ok {
				for bn, fn := range m.Blobs {
					if prefix != "" && !strings.HasPrefix(bn, prefix) {
						continue
					}
					var sz uint32
					if st, err := os.Stat(filepath.Join(ch.dir, fn)); err == nil {
						sz = uint32(st.Size())
					}
					list = append(list, blobEntry{name: bn, size: sz})
				}
				sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
			}
		}
		gs.mu.Unlock()
		for _, b := range list {
			nameBuf := append([]byte(b.name), 0)
			info := xGameSaveBlobInfoABI{
				Name: uintptr(unsafe.Pointer(&nameBuf[0])),
				Size: b.size,
			}
			more, _, _ := syscall.SyscallN(cb, uintptr(unsafe.Pointer(&info)), ctx)
			runtime.KeepAlive(nameBuf)
			runtime.KeepAlive(&info)
			if byte(more) == 0 {
				break
			}
		}
		return hresultOK
	}

	readWantedNames := func(blobNamesPtr uintptr, count uint32) []string {
		if blobNamesPtr < 0x10000 {
			return nil
		}
		ptrs := unsafe.Slice((*uintptr)(osPtr(blobNamesPtr)), count)
		out := make([]string, count)
		for i := uint32(0); i < count; i++ {
			out[i] = readSaveCStr(ptrs[i])
		}
		return out
	}

	return NewCOMObject("xgamesave", 32, map[int]uintptr{
		3: syscall.NewCallback(func(this, user, scid, syncOnDemand, outProv uintptr) uintptr { // 0x18 XGameSaveInitializeProvider
			gs.mu.Lock()
			nCont := len(gs.listContainersLocked(nil))
			gs.mu.Unlock()
			slog.Info("xgamesave!XGameSaveInitializeProvider -> stockage disque persistant", "dir", gs.rootDir, "containers", nCont)
			gpu.ReportBootProgress(0.90)
			if outProv >= 0x10000 {
				*(*uintptr)(osPtr(outProv)) = uintptr(gs.nextHandle.Add(1))
			}
			return hresultOK
		}),
		4: syscall.NewCallback(func(this, user, scid, syncOnDemand, asyncBlock uintptr) uintptr { // 0x20 XGameSaveInitializeProviderAsync
			gs.mu.Lock()
			nCont := len(gs.listContainersLocked(nil))
			gs.mu.Unlock()
			slog.Info("xgamesave!XGameSaveInitializeProviderAsync -> stockage disque persistant", "dir", gs.rootDir, "containers", nCont)
			gpu.ReportBootProgress(0.90)
			completeAsyncWith(asyncBlock, uint32(hresultOK), 8)
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
			gs.mu.Lock()
			rem := gsMaxQuotaBytes - gs.usedBytesLocked()
			gs.mu.Unlock()
			if rem < 0 {
				rem = 0
			}
			if outQuota >= 0x10000 {
				*(*int64)(osPtr(outQuota)) = rem
			}
			return hresultOK
		})),
		8: traceSlot("GetRemainingQuotaAsync", syscall.NewCallback(func(this, prov, asyncBlock uintptr) uintptr { // 0x40 XGameSaveGetRemainingQuotaAsync
			completeAsyncWith(asyncBlock, uint32(hresultOK), 8)
			return hresultOK
		})),
		9: traceSlot("GetRemainingQuotaResult", syscall.NewCallback(func(this, asyncBlock, outQuota uintptr) uintptr { // 0x48 XGameSaveGetRemainingQuotaResult
			gs.mu.Lock()
			rem := gsMaxQuotaBytes - gs.usedBytesLocked()
			gs.mu.Unlock()
			if rem < 0 {
				rem = 0
			}
			if outQuota >= 0x10000 {
				*(*int64)(osPtr(outQuota)) = rem
			}
			return hresultOK
		})),
		10: traceSlot("DeleteContainer", syscall.NewCallback(func(this, prov, namePtr uintptr) uintptr { // 0x50 XGameSaveDeleteContainer
			name := readSaveCStr(namePtr)
			if name != "" {
				gs.mu.Lock()
				_ = os.RemoveAll(gsContainerDir(gs.rootDir, name))
				gs.mu.Unlock()
				slog.Info("xgamesave!delete-container", "container", name)
			}
			return hresultOK
		})),
		11: traceSlot("DeleteContainerAsync", syscall.NewCallback(func(this, prov, namePtr, asyncBlock uintptr) uintptr { // 0x58 XGameSaveDeleteContainerAsync
			name := readSaveCStr(namePtr)
			if name != "" {
				gs.mu.Lock()
				_ = os.RemoveAll(gsContainerDir(gs.rootDir, name))
				gs.mu.Unlock()
				slog.Info("xgamesave!delete-container", "container", name)
			}
			completeAsync(asyncBlock)
			return hresultOK
		})),
		12: traceSlot("DeleteContainerResult", cbOK), // 0x60 XGameSaveDeleteContainerResult
		13: traceSlot("GetContainerInfo", syscall.NewCallback(func(this, prov, namePtr, ctx, cb uintptr) uintptr { // 0x68 XGameSaveGetContainerInfo
			want := readSaveCStr(namePtr)
			return invokeContainerCallbacks(func(n string) bool { return n == want }, ctx, cb)
		})),
		14: traceSlot("EnumerateContainerInfo", syscall.NewCallback(func(this, prov, ctx, cb uintptr) uintptr { // 0x70 XGameSaveEnumerateContainerInfo
			return invokeContainerCallbacks(nil, ctx, cb)
		})),
		15: traceSlot("EnumerateContainerInfoByName", syscall.NewCallback(func(this, prov, prefixPtr, ctx, cb uintptr) uintptr { // 0x78 XGameSaveEnumerateContainerInfoByName
			prefix := readSaveCStr(prefixPtr)
			if prefix == "" {
				return invokeContainerCallbacks(nil, ctx, cb)
			}
			return invokeContainerCallbacks(func(n string) bool { return strings.HasPrefix(n, prefix) }, ctx, cb)
		})),
		16: traceSlot("CreateContainer", syscall.NewCallback(func(this, prov, namePtr, outCont uintptr) uintptr { // 0x80 XGameSaveCreateContainer
			name := readSaveCStr(namePtr)
			if name == "" {
				return hresultInvalidArg
			}
			dir := gsContainerDir(gs.rootDir, name)
			h := uintptr(gs.nextHandle.Add(1))
			gs.mu.Lock()
			gs.containers[h] = &gsContainerHandle{name: name, dir: dir}
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
		18: traceSlot("EnumerateBlobInfo", syscall.NewCallback(func(this, cont, ctx, cb uintptr) uintptr { // 0x90 XGameSaveEnumerateBlobInfo
			return invokeBlobCallbacks(cont, "", ctx, cb)
		})),
		19: traceSlot("EnumerateBlobInfoByName", syscall.NewCallback(func(this, cont, prefixPtr, ctx, cb uintptr) uintptr { // 0x98 XGameSaveEnumerateBlobInfoByName
			return invokeBlobCallbacks(cont, readSaveCStr(prefixPtr), ctx, cb)
		})),
		20: traceSlot("ReadBlobData", syscall.NewCallback(func(this, cont, blobNames, countPtr, blobsSize, blobsBuf uintptr) uintptr { // 0xA0 XGameSaveReadBlobData
			if countPtr < 0x10000 {
				return hresultInvalidArg
			}
			wantedCount := *(*uint32)(osPtr(countPtr))
			wanted := readWantedNames(blobNames, wantedCount)
			gs.mu.Lock()
			ch := gs.containers[cont]
			var blobs []struct {
				name string
				data []byte
			}
			hr := hresultGSBlobNotFound
			if ch != nil {
				blobs, hr = gs.readBlobsLocked(ch, wanted)
			}
			gs.mu.Unlock()
			if hr != uint32(hresultOK) {
				return uintptr(hr)
			}
			packed := packGameSaveBlobs(blobs, blobsBuf)
			if blobsBuf < 0x10000 || uintptr(len(packed)) > blobsSize {
				return uintptr(hresultGSBufferTooSmall)
			}
			copy(unsafe.Slice((*byte)(osPtr(blobsBuf)), len(packed)), packed)
			*(*uint32)(osPtr(countPtr)) = uint32(len(blobs))
			return hresultOK
		})),
		21: traceSlot("ReadBlobDataAsync", syscall.NewCallback(func(this, cont, blobNames, count, asyncBlock uintptr) uintptr { // 0xA8 XGameSaveReadBlobDataAsync
			wanted := readWantedNames(blobNames, uint32(count))
			gs.mu.Lock()
			ch := gs.containers[cont]
			var blobs []struct {
				name string
				data []byte
			}
			hr := hresultGSBlobNotFound
			if ch != nil {
				blobs, hr = gs.readBlobsLocked(ch, wanted)
			}
			var packed []byte
			if hr == uint32(hresultOK) {
				packed = packGameSaveBlobs(blobs, 0)
			}
			gs.asyncReads[asyncBlock] = &gsAsyncReadResult{
				hr:     hr,
				packed: packed,
				count:  uint32(len(blobs)),
			}
			gs.mu.Unlock()
			completeAsyncWith(asyncBlock, hr, uint64(len(packed)))
			return hresultOK
		})),
		22: traceSlot("ReadBlobDataResult", syscall.NewCallback(func(this, asyncBlock, blobsSize, blobsBuf, outCount uintptr) uintptr { // 0xB0 XGameSaveReadBlobDataResult
			gs.mu.Lock()
			res := gs.asyncReads[asyncBlock]
			gs.mu.Unlock()
			if res == nil {
				if outCount >= 0x10000 {
					*(*uint32)(osPtr(outCount)) = 0
				}
				return uintptr(hresultGSBlobNotFound)
			}
			if res.hr != uint32(hresultOK) {
				gs.mu.Lock()
				delete(gs.asyncReads, asyncBlock)
				gs.mu.Unlock()
				if outCount >= 0x10000 {
					*(*uint32)(osPtr(outCount)) = 0
				}
				return uintptr(res.hr)
			}
			if blobsBuf < 0x10000 || uintptr(len(res.packed)) > blobsSize {
				// Conserve le résultat pour un nouvel essai avec un buffer plus grand
				return uintptr(hresultGSBufferTooSmall)
			}
			dst := unsafe.Slice((*byte)(osPtr(blobsBuf)), len(res.packed))
			copy(dst, res.packed)
			for i := uint32(0); i < res.count; i++ {
				base := int(i) * gsBlobStructSize
				nameOff := binary.LittleEndian.Uint64(dst[base : base+8])
				dataOff := binary.LittleEndian.Uint64(dst[base+16 : base+24])
				binary.LittleEndian.PutUint64(dst[base:base+8], uint64(blobsBuf)+nameOff)
				binary.LittleEndian.PutUint64(dst[base+16:base+24], uint64(blobsBuf)+dataOff)
			}
			if outCount >= 0x10000 {
				*(*uint32)(osPtr(outCount)) = res.count
			}
			gs.mu.Lock()
			delete(gs.asyncReads, asyncBlock)
			gs.mu.Unlock()
			return hresultOK
		})),
		23: traceSlot("CreateUpdate", syscall.NewCallback(func(this, cont, displayNamePtr, outUpd uintptr) uintptr { // 0xB8 XGameSaveCreateUpdate
			displayName := readSaveCStr(displayNamePtr)
			gs.mu.Lock()
			ch := gs.containers[cont]
			name := "default"
			dir := gsContainerDir(gs.rootDir, name)
			if ch != nil {
				name = ch.name
				dir = ch.dir
			}
			h := uintptr(gs.nextHandle.Add(1))
			gs.updates[h] = &gsUpdateHandle{name: name, dir: dir, displayName: displayName}
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
			bname := readSaveCStr(blobNamePtr)
			if bname == "" || byteCount > (16<<20) {
				return hresultInvalidArg
			}
			var buf []byte
			if byteCount > 0 && dataPtr >= 0x10000 {
				buf = make([]byte, byteCount)
				copy(buf, unsafe.Slice((*byte)(osPtr(dataPtr)), byteCount))
			}
			gs.mu.Lock()
			if u := gs.updates[upd]; u != nil {
				u.changes = append(u.changes, gsBlobChange{name: bname, data: buf})
			}
			gs.mu.Unlock()
			return hresultOK
		})),
		26: traceSlot("SubmitBlobDelete", syscall.NewCallback(func(this, upd, blobNamePtr uintptr) uintptr { // 0xD0 XGameSaveSubmitBlobDelete
			bname := readSaveCStr(blobNamePtr)
			if bname == "" {
				return hresultInvalidArg
			}
			gs.mu.Lock()
			if u := gs.updates[upd]; u != nil {
				u.changes = append(u.changes, gsBlobChange{name: bname, delete: true})
			}
			gs.mu.Unlock()
			return hresultOK
		})),
		27: traceSlot("SubmitUpdate", syscall.NewCallback(func(this, upd uintptr) uintptr { // 0xD8 XGameSaveSubmitUpdate
			gs.mu.Lock()
			u := gs.updates[upd]
			hr := uint32(hresultOK)
			if u != nil {
				hr = gs.applyUpdateLocked(u)
			}
			gs.mu.Unlock()
			return uintptr(hr)
		})),
		28: traceSlot("SubmitUpdateAsync", syscall.NewCallback(func(this, upd, asyncBlock uintptr) uintptr { // 0xE0 XGameSaveSubmitUpdateAsync
			gs.mu.Lock()
			u := gs.updates[upd]
			hr := uint32(hresultOK)
			if u != nil {
				hr = gs.applyUpdateLocked(u)
			}
			gs.asyncStatus[asyncBlock] = hr
			gs.mu.Unlock()
			completeAsyncWith(asyncBlock, hr, 0)
			return hresultOK
		})),
		29: traceSlot("SubmitUpdateResult", syscall.NewCallback(func(this, asyncBlock uintptr) uintptr { // 0xE8 XGameSaveSubmitUpdateResult
			gs.mu.Lock()
			hr, ok := gs.asyncStatus[asyncBlock]
			delete(gs.asyncStatus, asyncBlock)
			gs.mu.Unlock()
			if !ok {
				return hresultOK
			}
			return uintptr(hr)
		})),
	})
}
