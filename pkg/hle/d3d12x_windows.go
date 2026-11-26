//go:build windows

package hle

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/gpu"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/shader"
)

func buildFullscreenCompositeVS(psC *shader.Container) string {
	var inputs []shader.SigElement
	if psC != nil {
		if p, ok := psC.Part(shader.FourCCISG1); ok {
			inputs, _ = shader.ParseSignature(p.Data)
		}
	}
	var b strings.Builder
	b.WriteString("struct StageOut {\n")
	if len(inputs) == 0 {
		b.WriteString("    float4 pos : SV_Position;\n    float2 uv0 : TEXCOORD0;\n};\n")
		b.WriteString("StageOut VSMain(uint id : SV_VertexID) {\n    StageOut o;\n    float2 uv = float2((id << 1) & 2, id & 2);\n    o.pos = float4(uv * float2(2.0, -2.0) + float2(-1.0, 1.0), 0.0, 1.0);\n    o.uv0 = uv;\n    return o;\n}\n")
		return b.String()
	}
	for idx, el := range inputs {
		b.WriteString(fmt.Sprintf("    %s out%d : %s;\n", el.HLSLType(), idx, el.HLSLSemantic()))
	}
	b.WriteString("};\nStageOut VSMain(uint id : SV_VertexID) {\n    StageOut o;\n    float2 uv = float2((id << 1) & 2, id & 2);\n")
	for idx, el := range inputs {
		if strings.EqualFold(el.SemanticName, "SV_Position") {
			b.WriteString(fmt.Sprintf("    o.out%d = float4(uv * float2(2.0, -2.0) + float2(-1.0, 1.0), 0.0, 1.0);\n", idx))
		} else {
			switch el.ComponentCount() {
			case 1:
				b.WriteString(fmt.Sprintf("    o.out%d = uv.x;\n", idx))
			case 2:
				b.WriteString(fmt.Sprintf("    o.out%d = uv;\n", idx))
			case 3:
				b.WriteString(fmt.Sprintf("    o.out%d = float3(uv, 0.0);\n", idx))
			default:
				b.WriteString(fmt.Sprintf("    o.out%d = float4(uv, 0.0, 1.0);\n", idx))
			}
		}
	}
	b.WriteString("    return o;\n}\n")
	return b.String()
}

type guestPSO struct {
	vsHash     [16]byte
	psHash     [16]byte
	csHash     [16]byte
	hasVS      bool
	hasPS      bool
	hasCS      bool
	blend      *gpu.HostBlendState
	inputElems []gpu.HostInputElement
	vsCBSlot   uint32
	psCBSlot   uint32
	psSRVSlots [4]uint32
	psSRVCount int
	hostPSO    uintptr
	compiled   atomic.Bool
}

type fenceWaiter struct {
	val   uint64
	event uintptr
}

var k32SetEvent = k32.NewProc("SetEvent")

type winMemBasicInfo struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	PartitionId       uint16
	_pad0             uint16
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
	_pad1             uint32
}

// guestReadableLen retourne le nombre d'octets lisibles contigus (jusqu'à maxSz) à partir de addr.
func guestReadableLen(addr uintptr, maxSz uint64) uint64 {
	if addr < 0x10000 || addr >= 0x800000000000 || maxSz == 0 {
		return 0
	}
	targetEnd := addr + uintptr(maxSz)
	cur := addr
	for steps := 0; cur < targetEnd && steps < 512; steps++ {
		var mbi winMemBasicInfo
		r, _, _ := k32VirtualQuery.Call(cur, uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
		if r == 0 || mbi.State != 0x1000 /* MEM_COMMIT */ {
			break
		}
		prot := mbi.Protect & 0xFF
		if prot == 0x01 /* NOACCESS */ || mbi.Protect&0x100 /* PAGE_GUARD */ != 0 {
			break
		}
		end := mbi.BaseAddress + mbi.RegionSize
		if end <= cur {
			break
		}
		cur = end
	}
	if cur >= targetEnd {
		return maxSz
	}
	return uint64(cur - addr)
}

func isGuestReadable(addr uintptr, sz uint64) bool {
	return guestReadableLen(addr, sz) >= sz
}

func isGuestWritable(addr uintptr, sz uint64) bool {
	if addr < 0x10000 || sz == 0 {
		return false
	}
	targetEnd := addr + uintptr(sz)
	if targetEnd < addr {
		return false
	}
	type memoryBasicInfo struct {
		BaseAddress       uintptr
		AllocationBase    uintptr
		AllocationProtect uint32
		PartitionID       uint16
		_pad0             uint16
		RegionSize        uintptr
		State             uint32
		Protect           uint32
		Type              uint32
		_pad1             uint32
	}
	cur := addr
	for cur < targetEnd {
		var mbi memoryBasicInfo
		r, _, _ := k32VirtualQuery.Call(cur, uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
		if r == 0 || mbi.State != 0x1000 /* MEM_COMMIT */ {
			return false
		}
		prot := mbi.Protect & 0xFF
		writable := prot == 0x04 /* RW */ || prot == 0x08 /* WRITECOPY */ || prot == 0x40 /* ERW */ || prot == 0x80 /* EWC */
		if !writable || mbi.Protect&0x100 /* PAGE_GUARD */ != 0 {
			return false
		}
		end := mbi.BaseAddress + mbi.RegionSize
		if end <= cur {
			return false
		}
		cur = end
	}
	return true
}

// d3d12xState centralise les objets COM Xbox Series D3D12.X créés pendant le boot.
type d3d12xState struct {
	mu              sync.Mutex
	deviceObj       uintptr
	nextHeapBase    atomic.Uint64
	nextGPUAddr     atomic.Uint64
	fenceWaiters    map[uintptr][]fenceWaiter
	heapBases       map[uintptr]uint64
	resGPUAddrs     map[uintptr]uint64
	resDescs        map[uintptr]xgResourceDesc
	rtvResources    map[uintptr]uintptr
	psos            map[uintptr]*guestPSO
	dxbcBlobs       map[[16]byte][]byte
	activeHostPSO   uintptr
	activeHostCSPSO uintptr
	activeVBVA      uintptr
	activeVBSize    uint32
	activeVBStride  uint32
	activeIBVA      uintptr
	activeIBSize    uint32
	activeIBFormat  uint32
	activeTopology  uint32
	activeRTVCount  uint32
	activeRTVVA     uint64
	activeRootVA     uintptr
	activeRootSize   uint32
	activeCSRootVA   uintptr
	activeCSRootSize uint32
	activeCBSlot     uint32
	activePSCBSlot   uint32
	activePSSRVSlots [4]uint32
	activePSSRVCount int
	activeCBVSlots   [16]struct {
		va   uintptr
		size uint32
	}
	activeSRVSlots  [16]*gpu.HostTextureUpload
	activeSRVVA     uint64
	activeSRVTex    *gpu.HostTextureUpload
	frameDispatches []gpu.HostDispatchPass
	frameDraws      []gpu.HostDrawPass
	listDraws       map[uintptr][]gpu.HostDrawPass
	frameTexCache   map[uint64]*gpu.HostTextureUpload
	rtvSeenVAs      map[uint64]bool
	lastClearColor  [4]float32
	hasClearColor   bool
	queuesCreated   atomic.Int64
	listsCreated    atomic.Int64
	fencesCreated   atomic.Int64
	heapsCreated    atomic.Int64
	resCreated      atomic.Int64
	psoCreated      atomic.Int64
	execListsCount  atomic.Uint64
	drawCalls       atomic.Uint64
	dispatchCalls   atomic.Uint64
	clearRTVCalls   atomic.Uint64
}

func newD3D12XDevice() uintptr {
	st := &d3d12xState{
		fenceWaiters:   make(map[uintptr][]fenceWaiter),
		heapBases:      make(map[uintptr]uint64),
		resGPUAddrs:    make(map[uintptr]uint64),
		resDescs:       make(map[uintptr]xgResourceDesc),
		rtvResources:   make(map[uintptr]uintptr),
		psos:           make(map[uintptr]*guestPSO),
		dxbcBlobs:      make(map[[16]byte][]byte),
		listDraws:      make(map[uintptr][]gpu.HostDrawPass),
		frameTexCache:  make(map[uint64]*gpu.HostTextureUpload),
		rtvSeenVAs:     make(map[uint64]bool),
		lastClearColor: [4]float32{0.04, 0.05, 0.09, 1.0},
	}
	st.nextHeapBase.Store(0x0000100000000000)
	st.nextGPUAddr.Store(0x0000200000000000)

	gpuTrace := os.Getenv("ASTRAL_GPU_TRACE") == "1"
	dumpFrame := os.Getenv("ASTRAL_DUMP_FRAME") == "1"
	var introDumped atomic.Bool
	var nextScreenDumped atomic.Bool
	var scene3DDumped atomic.Bool
	var listSlotHits [160]atomic.Uint64

	cbOK := syscall.NewCallback(func() uintptr { return hresultOK })
	cbOne := syscall.NewCallback(func() uintptr { return 1 })
	cbGetDevice := syscall.NewCallback(func(this, iid, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = st.deviceObj
		}
		return hresultOK
	})

	// --- Objets DXGI (IDXGIDevice -> IDXGIAdapter -> IDXGIFactory / IDXGIOutput) ---
	dxgiOutputObj := NewCOMObject("IDXGIOutput", 24, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK,
		7: syscall.NewCallback(func(this, outDesc uintptr) uintptr { // GetDesc (DXGI_OUTPUT_DESC)
			if outDesc != 0 {
				// DesktopCoordinates = {0, 0, 1920, 1080}, AttachedToDesktop = 1
				b := unsafe.Slice((*byte)(osPtr(outDesc)), 96)
				for i := range b {
					b[i] = 0
				}
				*(*int32)(osPtr(outDesc + 64 + 8)) = 1920
				*(*int32)(osPtr(outDesc + 64 + 12)) = 1080
				*(*int32)(osPtr(outDesc + 80)) = 1
			}
			return hresultOK
		}),
		21: syscall.NewCallback(func(this, format, flags, countPtr, outModes uintptr) uintptr { // GetDisplayModeList (0xA8)
			if countPtr == 0 {
				return hresultInvalidArg
			}
			if outModes == 0 {
				*(*uint32)(osPtr(countPtr)) = 1
				return hresultOK
			}
			if *(*uint32)(osPtr(countPtr)) < 1 {
				return 0x887A0003 // DXGI_ERROR_MORE_DATA
			}
			*(*uint32)(osPtr(countPtr)) = 1
			// Mode (32 octets) : 1920x1080 @ 60/1 Hz, progressive (cohérent avec GetDesc)
			m := unsafe.Slice((*uint32)(osPtr(outModes)), 8)
			m[0], m[1], m[2], m[3], m[4], m[5], m[6], m[7] = 1920, 1080, 60, 1, uint32(format), 1, 0, 0
			return hresultOK
		}),
	})
	dxgiFactoryObj := NewCOMObject("IDXGIFactory", 32, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK,
	})
	dxgiAdapterObj := NewCOMObject("IDXGIAdapter", 24, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK,
		6: syscall.NewCallback(func(this, iid, outPtr uintptr) uintptr { // GetParent -> IDXGIFactory
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = dxgiFactoryObj
			}
			return hresultOK
		}),
		7: syscall.NewCallback(func(this, index, outPtr uintptr) uintptr { // EnumOutputs
			if uint32(index) != 0 {
				return 0x887A0002 // DXGI_ERROR_NOT_FOUND
			}
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = dxgiOutputObj
			}
			return hresultOK
		}),
		8: syscall.NewCallback(func(this, outDesc uintptr) uintptr { // GetDesc (DXGI_ADAPTER_DESC)
			if outDesc != 0 {
				b := unsafe.Slice((*byte)(osPtr(outDesc)), 312)
				for i := range b {
					b[i] = 0
				}
				name, _ := syscall.UTF16FromString("ASTRAL Xbox Series GPU")
				dst := unsafe.Slice((*uint16)(osPtr(outDesc)), len(name))
				copy(dst, name)
				// VendorId=AMD (0x1002), DedicatedVideoMemory=10 GiB
				*(*uint32)(osPtr(outDesc + 256)) = 0x1002
				*(*uint64)(osPtr(outDesc + 272)) = uint64(10) << 30
			}
			return hresultOK
		}),
	})
	dxgiDeviceObj := NewCOMObject("IDXGIDevice", 16, map[int]uintptr{
		0: syscall.NewCallback(func(this, iid, outPtr uintptr) uintptr {
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = st.deviceObj
			}
			return hresultOK
		}),
		3: cbOK, 4: cbOK, 5: cbOK,
		6: syscall.NewCallback(func(this, iid, outPtr uintptr) uintptr { // GetParent -> IDXGIAdapter
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = dxgiAdapterObj
			}
			return hresultOK
		}),
		7: syscall.NewCallback(func(this, outPtr uintptr) uintptr { // GetAdapter
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = dxgiAdapterObj
			}
			return hresultOK
		}),
	})

	cbDeviceQI := syscall.NewCallback(func(this, iid, outPtr uintptr) uintptr {
		if outPtr == 0 {
			return hresultInvalidArg
		}
		g := FormatGUID(iid)
		slog.Debug("ID3D12DeviceX!QueryInterface", "iid", g)
		if g == "{54EC77FA-1377-44E6-8C32-88FD5F44C84C}" ||
			g == "{77DB970F-6276-48BA-BA28-070143B4392C}" ||
			g == "{05008617-FBFD-4051-A790-144884B4F6A9}" {
			*(*uintptr)(osPtr(outPtr)) = dxgiDeviceObj
			return hresultOK
		}
		*(*uintptr)(osPtr(outPtr)) = st.deviceObj
		return hresultOK
	})

	signalFence := func(fenceObj uintptr, val uint64) {
		if fenceObj == 0 {
			return
		}
		// La valeur 64-bit du fence vit directement à fenceObj+0x10 (accessible CPU & GPU)
		atomic.StoreUint64((*uint64)(osPtr(fenceObj+0x10)), val)
		st.mu.Lock()
		ws := st.fenceWaiters[fenceObj]
		var remaining []fenceWaiter
		var toWake []uintptr
		for _, w := range ws {
			if val >= w.val {
				toWake = append(toWake, w.event)
			} else {
				remaining = append(remaining, w)
			}
		}
		if len(remaining) == 0 {
			delete(st.fenceWaiters, fenceObj)
		} else {
			st.fenceWaiters[fenceObj] = remaining
		}
		st.mu.Unlock()
		for _, ev := range toWake {
			if ev != 0 {
				_, _, _ = k32SetEvent.Call(ev)
			}
		}
	}

	// Helpers de création d'objets enfants D3D12.X
	cbQueueSignal := syscall.NewCallback(func(this, fenceObj, val uintptr) uintptr {
		signalFence(fenceObj, uint64(val))
		return hresultOK
	})
	cbQueueTimestampFreq := syscall.NewCallback(func(this, outFreq uintptr) uintptr {
		if outFreq == 0 {
			return hresultInvalidArg
		}
		*(*uint64)(osPtr(outFreq)) = 100_000_000 // 100 MHz (Xbox Series GPU clock)
		return hresultOK
	})
	var presents atomic.Uint64
	cbQueueExecuteLists := syscall.NewCallback(func(this, numLists, listsPtr uintptr) uintptr {
		n := uint32(numLists)
		st.execListsCount.Add(uint64(n))
		if n > 0 && n <= 256 && listsPtr >= 0x10000 {
			st.mu.Lock()
			for i := uint32(0); i < n; i++ {
				cl := *(*uintptr)(osPtr(listsPtr + uintptr(i)*8))
				if cl != 0 {
					if pending := st.listDraws[cl]; len(pending) > 0 {
						if len(st.frameDraws)+len(pending) <= 1024 {
							st.frameDraws = append(st.frameDraws, pending...)
						}
						delete(st.listDraws, cl)
					}
				}
			}
			st.mu.Unlock()
		}
		return hresultOK
	})
	cbQueuePresentX := syscall.NewCallback(func(this, numPlanes, planes, params uintptr) uintptr {
		n := presents.Add(1)
		var resObj uintptr
		var resW, resH, resFmt uint32
		if uint32(numPlanes) > 0 && planes >= 0x10000 {
			resCount := *(*uint32)(osPtr(planes + 8))
			ppRes := *(*uintptr)(osPtr(planes + 0x10))
			if resCount > 0 && ppRes >= 0x10000 {
				resObj = *(*uintptr)(osPtr(ppRes))
			}
		}
		st.mu.Lock()
		clr := st.lastClearColor
		draws := st.frameDraws
		st.frameDraws = nil
		clear(st.frameTexCache)
		dispatches := st.frameDispatches
		st.frameDispatches = nil
		var presentVA uint64
		if resObj != 0 {
			if d, ok := st.resDescs[resObj]; ok {
				resW, resH, resFmt = uint32(d.Width), d.Height, d.Format
			}
			presentVA = st.resGPUAddrs[resObj]
		}
		st.mu.Unlock()
		for i := range draws {
			draws[i].FrameNumber = uint32(n)
			draws[i].PassIndex = uint32(i)
		}
		if n == 1 || n%60 == 0 {
			slog.Info("ID3D12CommandQueueX!PresentX",
				"frame", n,
				"planes", uint32(numPlanes),
				"res", fmt.Sprintf("0x%X", resObj),
				"w", resW, "h", resH, "fmt", resFmt,
				"execLists", st.execListsCount.Load(),
				"draws", st.drawCalls.Load(),
				"dispatches", st.dispatchCalls.Load(),
				"clears", st.clearRTVCalls.Load(),
			)
			if gpuTrace && (n == 60 || n == 840) {
				var summary string
				for s := 8; s < 160; s++ {
					if c := listSlotHits[s].Load(); c > 0 {
						summary += fmt.Sprintf(" #%d=%d", s, c)
					}
				}
				slog.Info("GPU_TRACE CommandListX slots", "frame", n, "slots", summary, "psos", st.psoCreated.Load(), "heaps", st.heapsCreated.Load(), "res", st.resCreated.Load())
			}
		}
		if dumpFrame && ((len(draws) >= 8 && introDumped.CompareAndSwap(false, true)) || (introDumped.Load() && len(draws) == 5 && nextScreenDumped.CompareAndSwap(false, true))) {
			var sb string
			sb += fmt.Sprintf("=== FRAME %d: %d draws, %d dispatches, presentVA=0x%X clear=%v ===\n", n, len(draws), len(dispatches), presentVA, clr)
			for i, d := range draws {
				var texStr string
				if d.SRVTex != nil {
					texStr = fmt.Sprintf("tex=%dx%d fmt=%d swz=0x%X", d.SRVTex.Width, d.SRVTex.Height, d.SRVTex.Format, d.SRVTex.Swizzle)
				}
				sb += fmt.Sprintf("  draw[%d]: idx=%v topo=%d cnt=%d start=%d baseVtx=%d pso=0x%X stride=%d ibFmt=%d vb=%d ib=%d cb=%d tgt=0x%X srv=0x%X %s\n",
					i, d.Indexed, d.Topology, d.VertexCount, d.StartVertex, d.BaseVertex, d.HostPSO, d.VBStride, d.IBFormat, len(d.VBData), len(d.IBData), len(d.CBData), d.TargetVA, d.SRVGuestVA, texStr)
				if len(d.CBData) >= 96 {
					cf := unsafe.Slice((*float32)(unsafe.Pointer(&d.CBData[0])), 24)
					sb += fmt.Sprintf("    CB[0..15]:  (%.4f,%.4f,%.4f,%.4f) (%.4f,%.4f,%.4f,%.4f) (%.5f,%.5f,%.5f,%.5f) (%.5f,%.5f,%.5f,%.5f)\n",
						cf[0], cf[1], cf[2], cf[3], cf[4], cf[5], cf[6], cf[7], cf[8], cf[9], cf[10], cf[11], cf[12], cf[13], cf[14], cf[15])
					sb += fmt.Sprintf("    CB[16..23]: (%.5f,%.5f,%.5f,%.5f) (%.5f,%.5f,%.5f,%.5f)\n",
						cf[16], cf[17], cf[18], cf[19], cf[20], cf[21], cf[22], cf[23])
				}
				if !d.Indexed && d.VBStride >= 12 && d.VBStride%4 == 0 && len(d.VBData) >= int(d.VBStride) {
					nV := int(d.VertexCount)
					if nV > 6 {
						nV = 6
					}
					for v := 0; v < nV; v++ {
						off := (int(d.StartVertex) + v) * int(d.VBStride)
						if off+int(d.VBStride) > len(d.VBData) {
							break
						}
						fl := unsafe.Slice((*float32)(unsafe.Pointer(&d.VBData[off])), d.VBStride/4)
						sb += fmt.Sprintf("    VB[%d]: %v\n", v, fl)
					}
				}
				if d.Indexed && len(d.IBData) > 0 && d.VBStride >= 12 {
					elemSz := 2
					if d.IBFormat == 42 {
						elemSz = 4
					}
					nIdx := int(d.VertexCount)
					if nIdx > 30 {
						nIdx = 30
					}
					var idxs []uint32
					for k := 0; k < nIdx; k++ {
						ioff := (int(d.StartVertex) + k) * elemSz
						if ioff+elemSz > len(d.IBData) {
							break
						}
						if elemSz == 2 {
							idxs = append(idxs, uint32(*(*uint16)(unsafe.Pointer(&d.IBData[ioff]))))
						} else {
							idxs = append(idxs, *(*uint32)(unsafe.Pointer(&d.IBData[ioff])))
						}
					}
					sb += fmt.Sprintf("    IB[%d..]: %v\n", d.StartVertex, idxs)
					seen := map[uint32]bool{}
					for _, vi := range idxs {
						if seen[vi] || len(seen) >= 8 {
							continue
						}
						seen[vi] = true
						vIdx := int(vi) + int(d.BaseVertex)
						off := vIdx * int(d.VBStride)
						if off >= 0 && off+int(d.VBStride) <= len(d.VBData) {
							u0 := *(*uint32)(unsafe.Pointer(&d.VBData[off]))
							fl := unsafe.Slice((*float32)(unsafe.Pointer(&d.VBData[off])), d.VBStride/4)
							sb += fmt.Sprintf("    V[%d]: u0=0x%08X f=%v\n", vIdx, u0, fl)
						}
					}
				}
			}
			_ = os.WriteFile(os.TempDir()+"\\astral_intro_frame.txt", []byte(sb), 0o644)
			slog.Info("ASTRAL_DUMP_FRAME capturée", "frame", n, "draws", len(draws), "dispatches", len(dispatches))
		}
		// Capture diagnostique automatique d'une trame 3D (>= 80 draws) pour analyser le pipeline RAGE 9
		// (ExecuteIndirect, DispatchMesh, MRT, Depth, PSOs, slots CommandListX actifs).
		if len(draws) >= 80 && scene3DDumped.CompareAndSwap(false, true) {
			var slotsSummary string
			for s := 8; s < 160; s++ {
				if c := listSlotHits[s].Load(); c > 0 {
					slotsSummary += fmt.Sprintf(" #%d(0x%X)=%d", s, s*8, c)
				}
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "=== 3D SCENE FRAME %d: %d draws, %d dispatches, presentVA=0x%X ===\n", n, len(draws), len(dispatches), presentVA)
			fmt.Fprintf(&sb, "SLOTS:%s\n", slotsSummary)
			for i, d := range draws {
				if i >= 64 {
					break
				}
				var texStr string
				if d.SRVTex != nil {
					texStr = fmt.Sprintf("tex=%dx%d/fmt%d", d.SRVTex.Width, d.SRVTex.Height, d.SRVTex.Format)
				}
				fmt.Fprintf(&sb, "  draw[%02d]: clr=%v idx=%v topo=%d cnt=%d inst=%d pso=0x%X stride=%d vb=%d ib=%d cb=%d tgt=0x%X srv=0x%X %s\n",
					i, d.IsClear, d.Indexed, d.Topology, d.VertexCount, d.InstanceCount, d.HostPSO, d.VBStride, len(d.VBData), len(d.IBData), len(d.CBData), d.TargetVA, d.SRVGuestVA, texStr)
			}
			dumpPath := os.TempDir() + "\\astral_3d_frame.txt"
			_ = os.WriteFile(dumpPath, []byte(sb.String()), 0o644)
			slog.Info("ASTRAL_3D_FRAME capturée", "frame", n, "draws", len(draws), "path", dumpPath, "slots", slotsSummary)
		}
		gpu.PresentHostFrame(0, clr, presentVA, nil, 0, 0, 0, dispatches, draws...)
		return hresultOK
	})
	// Vtables partagées O(1) pour Queue, Allocator, DescriptorHeap, Resource, PSO et enfants génériques
	// (évite la limite de 2000 syscall.NewCallback du runtime Go lorsque le jeu crée des milliers de ressources).
	var pageMapLogged atomic.Bool
	cbQueueCopyPageMappings := syscall.NewCallback(func(this, countArg, batchesPtr, poolArg, poolPagesArg, flagsArg uintptr) uintptr {
		count := uint32(countArg)
		pool := uint64(poolArg)
		poolPages := uint32(poolPagesArg)
		if count == 0 || batchesPtr < 0x10000 {
			return hresultOK
		}
		type pageRange struct {
			kind  uint32
			start uint32
			count uint32
		}
		type pageBatch struct {
			destination uint64
			count       uint32
			_pad        uint32
			ranges      uintptr
		}
		const pageBytes = uint64(64 << 10)
		batches := unsafe.Slice((*pageBatch)(osPtr(batchesPtr)), count)
		var mappedPages uint64
		for _, b := range batches {
			if b.ranges < 0x10000 || b.count == 0 {
				continue
			}
			ranges := unsafe.Slice((*pageRange)(osPtr(b.ranges)), b.count)
			at := b.destination
			for _, r := range ranges {
				lenBytes := uint64(r.count) * pageBytes
				src := pool + uint64(r.start)*pageBytes
				switch r.kind {
				case 0: // plage contiguë depuis le pool
					if uint64(r.start)+uint64(r.count) <= uint64(poolPages) {
						if xmemMapAlias(at, src, lenBytes) {
							mappedPages += uint64(r.count)
						}
					}
				case 1: // page constante répétée r.count fois
					if r.start < poolPages {
						for k := uint64(0); k < uint64(r.count); k++ {
							if xmemMapAlias(at+k*pageBytes, src, pageBytes) {
								mappedPages++
							}
						}
					}
				case 2: // démapping
					_ = xmemMapAlias(at, 0, lenBytes)
				case 3: // inchangé
				}
				at += lenBytes
			}
		}
		if mappedPages > 0 && pageMapLogged.CompareAndSwap(false, true) {
			slog.Info("ID3D12CommandQueueX!CopyPageMappingsBatchX actif", "batches", count, "mappedPages", mappedPages, "pool", fmt.Sprintf("0x%X", pool))
		}
		return hresultOK
	})
	queueProto := NewCOMObjectWithHeader("ID3D12CommandQueueX", 48, 0x40, map[int]uintptr{
		3:  cbOK, // GetPrivateData
		4:  cbOK, // SetPrivateData
		5:  cbOK, // SetPrivateDataInterface
		6:  cbOK, // SetName
		7:  cbGetDevice,
		10: cbQueueExecuteLists,     // ExecuteCommandLists
		14: cbQueueSignal,           // Signal (2020)
		15: cbOK,                    // Wait (2020)
		16: cbQueueSignal,           // Signal (2026, 0x80)
		17: cbOK,                    // Wait (2026, 0x88)
		18: cbQueueTimestampFreq,    // GetTimestampFrequency (2026, 0x90)
		23: cbOK,                    // SuspendX (0xB8)
		24: cbOK,                    // ResumeX (0xC0)
		29: cbQueueCopyPageMappings, // CopyPageMappingsBatchX (2020)
		30: cbQueuePresentX,         // PresentX (2020, 0xF0)
		31: cbQueueCopyPageMappings, // CopyPageMappingsBatchX (2026, 0xF8)
		32: cbQueuePresentX,         // PresentX (2026, 0x100)
		33: cbQueueExecuteLists,     // ExecuteCommandListsX (2020)
		34: cbQueueSignal,           // Signal2X (2026, 0x110)
		35: cbQueueExecuteLists,     // ExecuteCommandListsX (2026, 0x118)
	})
	queueVtable := *(*uintptr)(osPtr(queueProto))
	newQueue := func() uintptr {
		st.queuesCreated.Add(1)
		mem, _, _ := k32VirtualAlloc.Call(0, 0x40, 0x3000, 0x04)
		if mem != 0 {
			*(*uintptr)(osPtr(mem)) = queueVtable
		}
		return mem
	}

	allocProto := NewCOMObject("ID3D12CommandAllocatorX", 16, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
		8: cbOK, // Reset
		9: cbOK, // ResetX / Free
	})
	allocVtable := *(*uintptr)(osPtr(allocProto))
	newAllocator := func() uintptr {
		mem, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if mem != 0 {
			*(*uintptr)(osPtr(mem)) = allocVtable
		}
		return mem
	}

	// Vtable partagée pour toutes les ID3D12GraphicsCommandListX (GTA VI en crée 581+ au boot).
	// En-tête de 0x1000 octets mis à zéro : PutPointer==LimitPointer==0 ET protège jusqu'au-delà de +0x564.
	cbListCopyMem := syscall.NewCallback(func(this, dst, src, length, flags uintptr) uintptr {
		if dst >= 0x10000 && src >= 0x10000 && length > 0 && length <= (256<<20) {
			copy(unsafe.Slice((*byte)(osPtr(dst)), length), unsafe.Slice((*byte)(osPtr(src)), length))
		}
		return 0
	})
	var copyTexHits atomic.Uint64
	cbListCopyTexRegion := syscall.NewCallback(func(this, pDst, dstX, dstY, dstZ, pSrc, pSrcBox uintptr) uintptr {
		listSlotHits[16].Add(1)
		n := copyTexHits.Add(1)
		if pDst < 0x10000 || pSrc < 0x10000 {
			return 0
		}
		dstRes := *(*uintptr)(osPtr(pDst))
		dstType := *(*uint32)(osPtr(pDst + 8))
		srcRes := *(*uintptr)(osPtr(pSrc))
		srcType := *(*uint32)(osPtr(pSrc + 8))
		st.mu.Lock()
		dstVA := st.resGPUAddrs[dstRes]
		dstDesc := st.resDescs[dstRes]
		srcVA := st.resGPUAddrs[srcRes]
		srcDesc := st.resDescs[srcRes]
		st.mu.Unlock()
		if n <= 8 {
			slog.Info("ID3D12GraphicsCommandListX!CopyTextureRegion",
				"n", n, "dstType", dstType, "srcType", srcType,
				"dstVA", fmt.Sprintf("0x%X", dstVA), "dstW", dstDesc.Width, "dstH", dstDesc.Height, "dstFmt", dstDesc.Format,
				"dstX", uint32(dstX), "dstY", uint32(dstY),
				"srcVA", fmt.Sprintf("0x%X", srcVA))
		}
		if dstVA < 0x10000 || srcVA < 0x10000 {
			return 0
		}
		// Copie PLACED_FOOTPRINT (Type=1) -> SUBRESOURCE_INDEX (Type=0) pour textures linéaires (ex: atlas glyphes Scaleform)
		if dstType == 0 && srcType == 1 && dstDesc.Width > 0 && dstDesc.Height > 0 {
			srcOff := *(*uint64)(osPtr(pSrc + 16))
			fpFmt := *(*uint32)(osPtr(pSrc + 24))
			fpW := *(*uint32)(osPtr(pSrc + 28))
			fpH := *(*uint32)(osPtr(pSrc + 32))
			fpPitch := *(*uint32)(osPtr(pSrc + 40))
			if fpFmt == 0 {
				fpFmt = dstDesc.Format
			}
			bpe, block, _ := gpu.DXGIFormatMetrics(fpFmt)
			if block == 1 && bpe > 0 && fpW > 0 && fpH > 0 && fpPitch > 0 {
				srcX0, srcY0 := uint32(0), uint32(0)
				cpW, cpH := fpW, fpH
				if pSrcBox >= 0x10000 {
					left := *(*uint32)(osPtr(pSrcBox + 0))
					top := *(*uint32)(osPtr(pSrcBox + 4))
					right := *(*uint32)(osPtr(pSrcBox + 12))
					bottom := *(*uint32)(osPtr(pSrcBox + 16))
					if right > left && bottom > top {
						srcX0, srcY0 = left, top
						cpW, cpH = right-left, bottom-top
					}
				}
				dstPitch := uint32(dstDesc.Width) * bpe
				rowBytes := cpW * bpe
				for row := uint32(0); row < cpH; row++ {
					dy := uint32(dstY) + row
					if dy >= dstDesc.Height {
						break
					}
					sAddr := uintptr(srcVA+srcOff) + uintptr(srcY0+row)*uintptr(fpPitch) + uintptr(srcX0*bpe)
					dAddr := uintptr(dstVA) + uintptr(dy)*uintptr(dstPitch) + uintptr(uint32(dstX)*bpe)
					if isGuestReadable(sAddr, uint64(rowBytes)) && isGuestReadable(dAddr, uint64(rowBytes)) {
						copy(unsafe.Slice((*byte)(osPtr(dAddr)), rowBytes), unsafe.Slice((*byte)(osPtr(sAddr)), rowBytes))
					}
				}
			}
		}
		_ = srcDesc
		return 0
	})
	cbListAllocUpload := syscall.NewCallback(func(this, size, align uintptr) uintptr {
		if size == 0 || size > (256<<20) {
			return 0
		}
		a := align
		if a < 16 {
			a = 16
		}
		// Allocation alignée dans la mémoire virtuelle OS (64 KiB aligné par VirtualAlloc)
		allocSize := (size + a + 4095) &^ 4095
		mem, _, _ := k32VirtualAlloc.Call(0, allocSize, 0x3000, 0x04)
		return mem
	})
	// Enregistre les slots réellement appelés sur ID3D12GraphicsCommandListX (une fois par slot)
	var listSlotLogged [160]atomic.Bool
	cmdListSlots := map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
	}
	for s := 8; s < 160; s++ {
		slotIdx := s
		cmdListSlots[s] = syscall.NewCallback(func() uintptr {
			listSlotHits[slotIdx].Add(1)
			if listSlotLogged[slotIdx].CompareAndSwap(false, true) {
				slog.Debug("ID3D12GraphicsCommandListX slot appelé", "slot", slotIdx, "offset", fmt.Sprintf("0x%X", slotIdx*8))
			}
			return hresultOK
		})
	}
	recordDrawPass := func(cmdList uintptr, indexed bool, count, instCount, startLoc uint32, baseVtx int32, startInst uint32) {
		st.mu.Lock()
		if len(st.listDraws[cmdList]) < 512 {
			pass := gpu.HostDrawPass{
				Indexed:       indexed,
				Topology:      st.activeTopology,
				VertexCount:   count,
				InstanceCount: instCount,
				StartVertex:   startLoc,
				BaseVertex:    baseVtx,
				StartInstance: startInst,
				HostPSO:       st.activeHostPSO,
				TargetVA:      st.activeRTVVA,
				SRVGuestVA:    st.activeSRVVA,
				VBStride:      st.activeVBStride,
				IBFormat:      st.activeIBFormat,
			}
			var ibElemSize uint32 = 2
			if st.activeIBFormat == 42 { // DXGI_FORMAT_R32_UINT
				ibElemSize = 4
			}
			var maxVtxIdx uint64
			hasMaxVtx := false
			if indexed && st.activeIBVA >= 0x10000 && st.activeIBSize > 0 {
				cpLen := st.activeIBSize
				if count > 0 {
					needIB := (uint64(startLoc) + uint64(count)) * uint64(ibElemSize)
					if needIB > 0 && needIB < uint64(cpLen) {
						cpLen = uint32(needIB)
					}
				}
				if cpLen > (1 << 20) {
					cpLen = 1 << 20
				}
				pass.IBData = make([]byte, cpLen)
				copy(pass.IBData, unsafe.Slice((*byte)(osPtr(st.activeIBVA)), cpLen))
				// Calcule l'indice de sommet maximal réellement référencé pour ne copier que la tranche utile du VB
				// (évite de copier 800 KiB de ring-buffer par draw et de déborder UploadVB 32 MiB -> triangles qui scintillent).
				startByte := uint64(startLoc) * uint64(ibElemSize)
				endByte := startByte + uint64(count)*uint64(ibElemSize)
				if count > 0 && endByte <= uint64(len(pass.IBData)) {
					if ibElemSize == 2 {
						for o := startByte; o+2 <= endByte; o += 2 {
							idx := uint64(pass.IBData[o]) | (uint64(pass.IBData[o+1]) << 8)
							if idx > maxVtxIdx {
								maxVtxIdx = idx
							}
						}
					} else {
						for o := startByte; o+4 <= endByte; o += 4 {
							idx := uint64(pass.IBData[o]) | (uint64(pass.IBData[o+1]) << 8) | (uint64(pass.IBData[o+2]) << 16) | (uint64(pass.IBData[o+3]) << 24)
							if idx > maxVtxIdx {
								maxVtxIdx = idx
							}
						}
					}
					hasMaxVtx = true
				}
			}
			if st.activeVBVA >= 0x10000 && st.activeVBSize > 0 {
				cpLen := st.activeVBSize
				if st.activeVBStride > 0 {
					if !indexed && count > 0 {
						needVB := (uint64(startLoc) + uint64(count)) * uint64(st.activeVBStride)
						if needVB > 0 && needVB < uint64(cpLen) {
							cpLen = uint32(needVB)
						}
					} else if indexed && hasMaxVtx {
						topVtx := int64(baseVtx) + int64(maxVtxIdx) + 1
						if topVtx > 0 {
							needVB := uint64(topVtx) * uint64(st.activeVBStride)
							if needVB > 0 && needVB < uint64(cpLen) {
								cpLen = uint32(needVB)
							}
						}
					}
				}
				if cpLen > (1 << 20) {
					cpLen = 1 << 20
				}
				pass.VBData = make([]byte, cpLen)
				copy(pass.VBData, unsafe.Slice((*byte)(osPtr(st.activeVBVA)), cpLen))
			}
			readCB := func(slot uint32) []byte {
				cbVA := st.activeRootVA
				cbSz := st.activeRootSize
				if slot < 16 && st.activeCBVSlots[slot].va >= 0x10000 {
					cbVA = st.activeCBVSlots[slot].va
					cbSz = st.activeCBVSlots[slot].size
				}
				if cbVA < 0x10000 {
					return nil
				}
				cbLen := cbSz
				if cbLen < 1024 {
					cbLen = 1024
				}
				if cbLen > 16384 {
					cbLen = 16384
				}
				avail := guestReadableLen(cbVA, uint64(cbLen))
				if avail == 0 {
					return nil
				}
				buf := make([]byte, avail)
				copy(buf, unsafe.Slice((*byte)(osPtr(cbVA)), avail))
				return buf
			}
			pass.CBData = readCB(st.activeCBSlot)
			if st.activePSCBSlot != st.activeCBSlot {
				pass.PSCBData = readCB(st.activePSCBSlot)
			}
			cloneSRV := func(src *gpu.HostTextureUpload) *gpu.HostTextureUpload {
				if src == nil || st.rtvSeenVAs[src.GuestVA] {
					return nil
				}
				if cached, ok := st.frameTexCache[src.GuestVA]; ok {
					return cached
				}
				tx := *src
				if tx.GuestVA >= 0x10000 && tx.Width > 0 && tx.Height > 0 && tx.Width <= 4096 && tx.Height <= 4096 {
					bpe, block, _ := gpu.DXGIFormatMetrics(tx.Format)
					sz := gpu.TiledSizeBytes(tx.Width, tx.Height, bpe, block, tx.Swizzle)
					if sz > 0 && sz <= (16<<20) && isGuestReadable(uintptr(tx.GuestVA), sz) {
						pix := make([]byte, sz)
						copy(pix, unsafe.Slice((*byte)(osPtr(uintptr(tx.GuestVA))), sz))
						tx.Pixels = pix
						st.frameTexCache[src.GuestVA] = &tx
						return &tx
					}
				}
				return nil
			}
			if st.activePSSRVCount > 0 {
				for si := 0; si < st.activePSSRVCount && si < 4; si++ {
					gSlot := st.activePSSRVSlots[si]
					var src *gpu.HostTextureUpload
					if gSlot < 16 {
						src = st.activeSRVSlots[gSlot]
					}
					if si == 0 {
						if src == nil {
							src = st.activeSRVTex
						}
						if src != nil {
							pass.SRVGuestVA = src.GuestVA
							pass.SRVTex = cloneSRV(src)
						}
					} else if src != nil {
						pass.ExtraSRVGuestVAs[si-1] = src.GuestVA
						pass.ExtraSRVs[si-1] = cloneSRV(src)
					}
				}
			} else if st.activeSRVTex != nil {
				pass.SRVTex = cloneSRV(st.activeSRVTex)
			}
			st.listDraws[cmdList] = append(st.listDraws[cmdList], pass)
		}
		st.mu.Unlock()
	}
	cbListReset := syscall.NewCallback(func(this, alloc, initPSO uintptr) uintptr {
		listSlotHits[10].Add(1)
		st.mu.Lock()
		delete(st.listDraws, this)
		st.mu.Unlock()
		return hresultOK
	})
	cbListDrawInst := syscall.NewCallback(func(this, vtxCount, instCount, startVtx, startInst uintptr) uintptr {
		listSlotHits[12].Add(1)
		n := st.drawCalls.Add(1)
		recordDrawPass(this, false, uint32(vtxCount), uint32(instCount), uint32(startVtx), 0, uint32(startInst))
		if gpuTrace && n <= 12 {
			slog.Info("GPU_TRACE DrawInstanced", "n", n, "vtx", uint32(vtxCount), "inst", uint32(instCount), "startVtx", uint32(startVtx), "startInst", uint32(startInst))
		}
		return 0
	})
	cbListDrawIdx := syscall.NewCallback(func(this, idxCount, instCount, startIdx, baseVtx, startInst uintptr) uintptr {
		listSlotHits[13].Add(1)
		n := st.drawCalls.Add(1)
		recordDrawPass(this, true, uint32(idxCount), uint32(instCount), uint32(startIdx), int32(baseVtx), uint32(startInst))
		if gpuTrace && n <= 12 {
			slog.Info("GPU_TRACE DrawIndexedInstanced", "n", n, "idx", uint32(idxCount), "inst", uint32(instCount), "startIdx", uint32(startIdx), "baseVtx", int32(baseVtx))
		}
		return 0
	})
	cbListDrawIdxX := syscall.NewCallback(func(this, idxCount, instCount, startIdx, baseVtx, startInst uintptr) uintptr {
		listSlotHits[81].Add(1)
		n := st.drawCalls.Add(1)
		recordDrawPass(this, true, uint32(idxCount), uint32(instCount), uint32(startIdx), int32(baseVtx), uint32(startInst))
		if gpuTrace && n <= 12 {
			slog.Info("GPU_TRACE DrawIndexedX", "n", n, "idx", uint32(idxCount), "inst", uint32(instCount), "startIdx", uint32(startIdx))
		}
		return 0
	})
	cbListDispatch := syscall.NewCallback(func(this, x, y, z uintptr) uintptr {
		listSlotHits[14].Add(1)
		st.dispatchCalls.Add(1)
		st.mu.Lock()
		if len(st.frameDispatches) < 32 && st.activeHostCSPSO != 0 {
			dp := gpu.HostDispatchPass{
				X:       uint32(x),
				Y:       uint32(y),
				Z:       uint32(z),
				HostPSO: st.activeHostCSPSO,
			}
			csVA := st.activeCSRootVA
			csSz := st.activeCSRootSize
			if csVA < 0x10000 {
				csVA = st.activeRootVA
				csSz = st.activeRootSize
			}
			if csVA >= 0x10000 {
				if csSz < 256 {
					csSz = 256
				}
				if csSz > 4096 {
					csSz = 4096
				}
				if isGuestReadable(csVA, uint64(csSz)) {
					dp.CBData = make([]byte, csSz)
					copy(dp.CBData, unsafe.Slice((*byte)(osPtr(csVA)), csSz))
				}
			}
			st.frameDispatches = append(st.frameDispatches, dp)
		}
		st.mu.Unlock()
		return 0
	})
	cbListClearRTV := syscall.NewCallback(func(this, rtvHandle, colorPtr, numRects, rects uintptr) uintptr {
		listSlotHits[52].Add(1)
		n := st.clearRTVCalls.Add(1)
		if colorPtr >= 0x10000 {
			c := *(*[4]float32)(osPtr(colorPtr))
			st.mu.Lock()
			st.lastClearColor = c
			st.hasClearColor = true
			var rtvVA uint64
			if res := st.rtvResources[rtvHandle]; res != 0 {
				rtvVA = st.resGPUAddrs[res]
			} else if rtvHandle >= 0x10000 && rtvHandle < 0x800000000000 && *(*uint32)(osPtr(rtvHandle)) == 0x52545631 {
				if res := *(*uintptr)(osPtr(rtvHandle + 8)); res != 0 {
					rtvVA = st.resGPUAddrs[res]
				}
			}
			if rtvVA != 0 && len(st.listDraws[this]) < 512 {
				st.listDraws[this] = append(st.listDraws[this], gpu.HostDrawPass{
					IsClear:    true,
					TargetVA:   rtvVA,
					ClearColor: c,
				})
			}
			st.mu.Unlock()
			if gpuTrace && n <= 8 {
				slog.Info("GPU_TRACE ClearRenderTargetView", "n", n, "rtv", fmt.Sprintf("0x%X", rtvHandle), "va", fmt.Sprintf("0x%X", rtvVA), "rgba", c)
			}
		}
		return 0
	})
	describeDXBCBytes := func(data []byte) string {
		if len(data) < 32 {
			return "none"
		}
		c, err := shader.ParseDXBC(data)
		if err != nil {
			return fmt.Sprintf("len=%d err=%v", len(data), err)
		}
		var parts string
		for _, pt := range c.Parts {
			parts += pt.FourCC.String() + ","
		}
		progs, perr := shader.ExtractNativePrograms(c)
		if perr != nil || len(progs) == 0 {
			return fmt.Sprintf("len=%d parts=[%s] nativeErr=%v", len(data), parts, perr)
		}
		var nat string
		for _, pr := range progs {
			insts := shader.DecodeGFX10(pr.Code)
			nat += fmt.Sprintf("%s(%dB/%dins/w64=%v/v%d/s%d/res=%d) ", pr.Stage, len(pr.Code), len(insts), pr.Wave64, pr.VGPRs, pr.SGPRs, len(pr.UsedResources))
		}
		return fmt.Sprintf("len=%d parts=[%s] native=[%s]", len(data), parts, nat)
	}
	psoWorkers := runtime.GOMAXPROCS(0)
	if psoWorkers > 6 {
		psoWorkers = 6
	}
	if psoWorkers < 2 {
		psoWorkers = 2
	}
	psoCompileSem := make(chan struct{}, psoWorkers)
	compileGuestPSO := func(psoObj uintptr, gp *guestPSO) {
		if gp.hasCS && !gp.hasVS {
			// Ne pas compiler/exécuter les Compute PSO invités sur le GPU hôte tant que leur UAV
			// n'est pas relié aux passes graphiques : les gros CS 3D (2000-4000 ins) saturaient
			// D3DCompile et provoquaient des TDR GPU (2s/trame -> watchdog 0x4D8BD421).
			return
		}
		if !gp.hasVS || !gp.hasPS {
			return
		}
		st.mu.Lock()
		vsData := st.dxbcBlobs[gp.vsHash]
		psData := st.dxbcBlobs[gp.psHash]
		st.mu.Unlock()
		if len(vsData) < 32 || len(psData) < 32 {
			return
		}
		runGraphicsCompile := func(asyncLarge bool) {
			if gpuTrace {
				slog.Info("GPU_TRACE Active PSO bound",
					"pso", fmt.Sprintf("0x%X", psoObj),
					"vs", describeDXBCBytes(vsData),
					"ps", describeDXBCBytes(psData),
				)
			}
			vsC, err := shader.ParseDXBC(vsData)
			if err != nil {
				slog.Warn("ParseDXBC VS échoué", "pso", fmt.Sprintf("0x%X", psoObj), "err", err)
				return
			}
			psC, err := shader.ParseDXBC(psData)
			if err != nil {
				slog.Warn("ParseDXBC PS échoué", "pso", fmt.Sprintf("0x%X", psoObj), "err", err)
				return
			}
			vsProgs, err := shader.ExtractNativePrograms(vsC)
			if err != nil || len(vsProgs) == 0 {
				return
			}
			psProgs, err := shader.ExtractNativePrograms(psC)
			if err != nil || len(psProgs) == 0 {
				return
			}
			vsOut, err := shader.EmitHLSL(vsC, vsProgs[0])
			if err != nil {
				return
			}
			psOut, err := shader.EmitHLSL(psC, psProgs[0])
			if err != nil {
				return
			}
			var inElems []gpu.HostInputElement
			if len(gp.inputElems) > 0 {
				inElems = gp.inputElems
			} else {
				for _, el := range vsOut.InputElements {
					inElems = append(inElems, gpu.HostInputElement{
						SemanticName:  el.SemanticName,
						SemanticIndex: el.SemanticIndex,
						Format:        el.DXGIFormat(),
					})
				}
			}
			finishGraphicsPSO := func() {
				var hPSO uintptr
				var cerr error
				isGammaComposite := vsOut.TotalInsts == 45 && psOut.TotalInsts >= 60 && len(psOut.SRVDescriptorSlots) == 1
				if isGammaComposite {
					// Le VS 45/45 lit une 2e matrice CBV absente ; on l'associe à un VS triangle plein écran
					// tout en exécutant le vrai PS 70/70 recompilé (courbe gamma / luminosité).
					fsVS := buildFullscreenCompositeVS(psC)
					opaqueBlend := &gpu.HostBlendState{
						BlendEnable: false, SrcBlend: 2, DestBlend: 1, BlendOp: 1,
						SrcBlendAlpha: 2, DestBlendAlpha: 1, BlendOpAlpha: 1, WriteMask: 0x0F,
					}
					hPSO, cerr = gpu.CompileHostPSOWithBlend(fsVS, "VSMain", psOut.Source, psOut.EntryPoint, nil, opaqueBlend)
					if cerr == nil && hPSO != 0 {
						gpu.RegisterGammaCompositePSO(hPSO)
						slog.Info("PSO composite gamma/luminosité plein écran activé (PS réel 70/70)", "guestPSO", fmt.Sprintf("0x%X", psoObj), "hostPSO", fmt.Sprintf("0x%X", hPSO))
					}
				} else {
					hPSO, cerr = gpu.CompileHostPSOWithBlend(vsOut.Source, vsOut.EntryPoint, psOut.Source, psOut.EntryPoint, inElems, gp.blend)
				}
				if cerr != nil {
					slog.Warn("Recompilation PSO RDNA2->HLSL->D3D12 échouée (repli actif)", "pso", fmt.Sprintf("0x%X", psoObj), "err", cerr)
					return
				}
				st.mu.Lock()
				gp.hostPSO = hPSO
				gp.vsCBSlot = vsOut.CBDescriptorSlot
				gp.psCBSlot = psOut.CBDescriptorSlot
				gp.psSRVCount = len(psOut.SRVDescriptorSlots)
				if gp.psSRVCount > 4 {
					gp.psSRVCount = 4
				}
				for i := 0; i < gp.psSRVCount; i++ {
					gp.psSRVSlots[i] = psOut.SRVDescriptorSlots[i]
				}
				st.mu.Unlock()
				gpu.ReportWorldShaderCompiled()
				blendDesc := "default"
				if gp.blend != nil {
					blendDesc = fmt.Sprintf("en=%v src=%d dst=%d op=%d", gp.blend.BlendEnable, gp.blend.SrcBlend, gp.blend.DestBlend, gp.blend.BlendOp)
				}
				ilDescStr := "none"
				if len(gp.inputElems) > 0 {
					var parts []string
					for _, el := range gp.inputElems {
						parts = append(parts, fmt.Sprintf("%s%d:fmt%d@%d/s%d", el.SemanticName, el.SemanticIndex, el.Format, el.AlignedByteOffset, el.InputSlot))
					}
					ilDescStr = strings.Join(parts, ",")
				}
				slog.Info("PSO RDNA2 recompilé en ID3D12PipelineState matériel",
					"guestPSO", fmt.Sprintf("0x%X", psoObj),
					"hostPSO", fmt.Sprintf("0x%X", hPSO),
					"vs", fmt.Sprintf("%d/%d ins", vsOut.LoweredInsts, vsOut.TotalInsts),
					"ps", fmt.Sprintf("%d/%d ins", psOut.LoweredInsts, psOut.TotalInsts),
					"missingPS", psOut.MissingOps,
					"blend", blendDesc,
					"guestIL", ilDescStr,
					"vsCBSlot", vsOut.CBDescriptorSlot,
					"psCBSlot", psOut.CBDescriptorSlot,
					"psSRVs", psOut.SRVDescriptorSlots,
				)
				if dumpFrame {
					_ = os.WriteFile(fmt.Sprintf("%s\\astral_pso_%X_vs.hlsl", os.TempDir(), hPSO), []byte(vsOut.Source), 0o644)
					_ = os.WriteFile(fmt.Sprintf("%s\\astral_pso_%X_ps.hlsl", os.TempDir(), hPSO), []byte(psOut.Source), 0o644)
				}
			}
			if asyncLarge && vsOut.TotalInsts+psOut.TotalInsts > 160 {
				go func() {
					psoCompileSem <- struct{}{}
					defer func() { <-psoCompileSem }()
					finishGraphicsPSO()
				}()
			} else {
				finishGraphicsPSO()
			}
			if gpuTrace {
				slog.Info("GPU_TRACE PSO missing ops", "guestPSO", fmt.Sprintf("0x%X", psoObj), "vsMissing", vsOut.MissingOps, "psMissing", psOut.MissingOps)
			}
		}
		// Après l'UI initiale (> 5 000 draws), toute compilation de PSO 3D (ParseDXBC+EmitHLSL+D3DCompile)
		// passe en arrière-plan pour garantir que le thread de rendu ne dépasse jamais le watchdog 30s.
		if st.drawCalls.Load() > 5000 {
			go func() {
				psoCompileSem <- struct{}{}
				defer func() { <-psoCompileSem }()
				runGraphicsCompile(false)
			}()
		} else {
			runGraphicsCompile(true)
		}
	}
	cbListSetPSO := syscall.NewCallback(func(this, pso uintptr) uintptr {
		listSlotHits[25].Add(1)
		if pso != 0 {
			st.mu.Lock()
			gp := st.psos[pso]
			st.mu.Unlock()
			if gp != nil {
				if gp.compiled.CompareAndSwap(false, true) {
					compileGuestPSO(pso, gp)
				}
				st.mu.Lock()
				if gp.hasCS && !gp.hasVS {
					st.activeHostCSPSO = gp.hostPSO
				} else {
					st.activeHostPSO = gp.hostPSO
					st.activeCBSlot = gp.vsCBSlot
					st.activePSCBSlot = gp.psCBSlot
					st.activePSSRVSlots = gp.psSRVSlots
					st.activePSSRVCount = gp.psSRVCount
				}
				st.mu.Unlock()
			}
		}
		return 0
	})
	cbListSetRootTable := syscall.NewCallback(func(this, rootIdx, baseDesc uintptr) uintptr {
		listSlotHits[36].Add(1)
		if baseDesc >= 0x10000 && baseDesc < 0x800000000000 {
			st.mu.Lock()
			foundSRV := false
			for s := uintptr(0); s < 16; s++ {
				slotPtr := baseDesc + s*32
				if s >= 8 && !isGuestReadable(slotPtr, 32) {
					break
				}
				tag := *(*uint32)(osPtr(slotPtr))
				switch tag {
				case 0x43425631: // "CBV1" : { tag u32, _pad u32, BufferLocation u64 (+8), SizeInBytes u32 (+16) }
					cbVA := *(*uintptr)(osPtr(slotPtr + 8))
					cbSz := *(*uint32)(osPtr(slotPtr + 16))
					if cbVA >= 0x10000 && cbVA < 0x800000000000 {
						st.activeCBVSlots[s].va = cbVA
						st.activeCBVSlots[s].size = cbSz
						if s == 0 {
							st.activeRootVA = cbVA
							st.activeRootSize = cbSz
						}
					}
				case 0x53525632: // "SRV2"
					dim := *(*uint32)(osPtr(slotPtr + 24))
					va := *(*uint64)(osPtr(slotPtr + 8))
					w := *(*uint32)(osPtr(slotPtr + 16))
					h := *(*uint32)(osPtr(slotPtr + 20))
					fmtID := *(*uint32)(osPtr(slotPtr + 4))
					swz := *(*uint32)(osPtr(slotPtr + 28))
					if (dim == 2 || dim == 3) && va >= 0x10000 && va < 0x800000000000 && w > 0 {
						if h == 0 {
							h = 1
						}
						tx := &gpu.HostTextureUpload{
							GuestVA: va,
							Width:   w,
							Height:  h,
							Format:  fmtID,
							Swizzle: swz,
						}
						st.activeSRVSlots[s] = tx
						if !foundSRV {
							st.activeSRVVA = va
							st.activeSRVTex = tx
							foundSRV = true
						}
					}
				}
			}
			st.mu.Unlock()
		}
		return 0
	})
	cbListSetRootCBV := syscall.NewCallback(func(this, rootIdx, gpuVA uintptr) uintptr {
		c := listSlotHits[42].Add(1)
		if gpuVA >= 0x10000 && gpuVA < 0x800000000000 {
			st.mu.Lock()
			st.activeRootVA = gpuVA
			st.activeRootSize = 4096
			if rootIdx < 8 {
				st.activeCBVSlots[rootIdx].va = gpuVA
				st.activeCBVSlots[rootIdx].size = 4096
			}
			st.mu.Unlock()
		}
		if gpuTrace && c <= 8 {
			slog.Info("GPU_TRACE SetGraphicsRootCBV", "n", c, "rootIdx", uint32(rootIdx), "gpuVA", fmt.Sprintf("0x%X", gpuVA))
		}
		return 0
	})
	cbListSetRootSRV := syscall.NewCallback(func(this, rootIdx, gpuVA uintptr) uintptr {
		c := listSlotHits[44].Add(1)
		if gpuVA >= 0x10000 && gpuVA < 0x800000000000 {
			st.mu.Lock()
			st.activeRootVA = gpuVA
			st.mu.Unlock()
		}
		if gpuTrace && c <= 8 {
			slog.Info("GPU_TRACE SetGraphicsRootSRV", "n", c, "rootIdx", uint32(rootIdx), "gpuVA", fmt.Sprintf("0x%X", gpuVA))
		}
		return 0
	})
	cbListSetVB := syscall.NewCallback(func(this, startSlot, numViews, viewsPtr uintptr) uintptr {
		c := listSlotHits[48].Add(1)
		if uint32(numViews) > 0 && viewsPtr >= 0x10000 {
			// D3D12_VERTEX_BUFFER_VIEW : { BufferLocation u64 (+0), SizeInBytes u32 (+8), StrideInBytes u32 (+12) }
			loc := *(*uint64)(osPtr(viewsPtr + 0))
			sz := *(*uint32)(osPtr(viewsPtr + 8))
			stride := *(*uint32)(osPtr(viewsPtr + 12))
			if loc >= 0x10000 && loc < 0x800000000000 && sz > 0 && stride > 0 {
				st.mu.Lock()
				st.activeVBVA = uintptr(loc)
				st.activeVBSize = sz
				st.activeVBStride = stride
				st.mu.Unlock()
			}
			if gpuTrace && c <= 8 {
				slog.Info("GPU_TRACE IASetVertexBuffers", "n", c, "slot", uint32(startSlot), "views", uint32(numViews), "va", fmt.Sprintf("0x%X", loc), "size", sz, "stride", stride)
			}
		}
		return 0
	})
	cbListSetIB := syscall.NewCallback(func(this, viewPtr uintptr) uintptr {
		listSlotHits[47].Add(1)
		if viewPtr >= 0x10000 {
			loc := *(*uint64)(osPtr(viewPtr + 0))
			sz := *(*uint32)(osPtr(viewPtr + 8))
			ibFmt := *(*uint32)(osPtr(viewPtr + 12))
			if loc >= 0x10000 && loc < 0x800000000000 && sz > 0 {
				st.mu.Lock()
				st.activeIBVA = uintptr(loc)
				st.activeIBSize = sz
				st.activeIBFormat = ibFmt
				st.mu.Unlock()
			}
		}
		return 0
	})
	cbListSetTopo := syscall.NewCallback(func(this, topo uintptr) uintptr {
		listSlotHits[20].Add(1)
		st.mu.Lock()
		st.activeTopology = uint32(topo)
		st.mu.Unlock()
		return 0
	})
	cbListSetRTVs := syscall.NewCallback(func(this, numRTVs, rtvsPtr, single, dsvPtr uintptr) uintptr {
		listSlotHits[50].Add(1)
		var rtvVA uint64
		if uint32(numRTVs) > 0 && rtvsPtr >= 0x10000 && rtvsPtr < 0x800000000000 {
			rtvHandle := *(*uintptr)(osPtr(rtvsPtr))
			var res uintptr
			st.mu.Lock()
			if r, ok := st.rtvResources[rtvHandle]; ok {
				res = r
			} else if rtvHandle >= 0x10000 && rtvHandle < 0x800000000000 && *(*uint32)(osPtr(rtvHandle)) == 0x52545631 {
				res = *(*uintptr)(osPtr(rtvHandle + 8))
			}
			if res != 0 {
				rtvVA = st.resGPUAddrs[res]
			}
			st.activeRTVCount = uint32(numRTVs)
			st.activeRTVVA = rtvVA
			if rtvVA != 0 {
				st.rtvSeenVAs[rtvVA] = true
			}
			st.mu.Unlock()
			return 0
		}
		st.mu.Lock()
		st.activeRTVCount = uint32(numRTVs)
		st.activeRTVVA = 0
		st.mu.Unlock()
		return 0
	})
	cbListWrite32 := syscall.NewCallback(func(this, addr, val, flush, flags uintptr) uintptr {
		listSlotHits[71].Add(1)
		if addr >= 0x10000 && addr < 0x800000000000 {
			atomic.StoreUint32((*uint32)(osPtr(addr)), uint32(val))
		}
		return 0
	})
	cbListWrite64 := syscall.NewCallback(func(this, addr, val, flush, flags uintptr) uintptr {
		listSlotHits[72].Add(1)
		if addr >= 0x10000 && addr < 0x800000000000 {
			v := uint64(val)
			if uint32(flags) == 1 || uint32(flags) == 2 { // GPU_TIMESTAMP / SYSTEM_TIMESTAMP
				v = uint64(time.Now().UnixNano() / 100) // ticks 10 MHz
			}
			atomic.StoreUint64((*uint64)(osPtr(addr)), v)
		}
		return 0
	})
	cbListSetComputeRootTable := syscall.NewCallback(func(this, rootIdx, baseDesc uintptr) uintptr {
		listSlotHits[35].Add(1)
		if baseDesc >= 0x10000 && baseDesc < 0x800000000000 {
			st.mu.Lock()
			for s := uintptr(0); s < 8; s++ {
				slotPtr := baseDesc + s*32
				if *(*uint32)(osPtr(slotPtr)) == 0x43425631 { // "CBV1"
					cbVA := *(*uintptr)(osPtr(slotPtr + 8))
					cbSz := *(*uint32)(osPtr(slotPtr + 16))
					if cbVA >= 0x10000 && cbVA < 0x800000000000 {
						st.activeCSRootVA = cbVA
						st.activeCSRootSize = cbSz
						break
					}
				}
			}
			st.mu.Unlock()
		}
		return 0
	})
	cbListSetComputeRootCBV := syscall.NewCallback(func(this, rootIdx, gpuVA uintptr) uintptr {
		listSlotHits[41].Add(1)
		if gpuVA >= 0x10000 && gpuVA < 0x800000000000 {
			st.mu.Lock()
			st.activeCSRootVA = gpuVA
			st.activeCSRootSize = 4096
			st.mu.Unlock()
		}
		return 0
	})
	cbListSetComputeRootSRV := syscall.NewCallback(func(this, rootIdx, gpuVA uintptr) uintptr {
		listSlotHits[43].Add(1)
		if gpuVA >= 0x10000 && gpuVA < 0x800000000000 {
			st.mu.Lock()
			if st.activeCSRootVA == 0 {
				st.activeCSRootVA = gpuVA
				st.activeCSRootSize = 4096
			}
			st.mu.Unlock()
		}
		return 0
	})
	cmdListSlots[10] = cbListReset               // Reset
	cmdListSlots[12] = cbListDrawInst            // DrawInstanced
	cmdListSlots[13] = cbListDrawIdx             // DrawIndexedInstanced
	cmdListSlots[14] = cbListDispatch            // Dispatch
	cmdListSlots[16] = cbListCopyTexRegion       // CopyTextureRegion
	cmdListSlots[20] = cbListSetTopo             // IASetPrimitiveTopology
	cmdListSlots[25] = cbListSetPSO              // SetPipelineState
	cmdListSlots[35] = cbListSetComputeRootTable // SetComputeRootDescriptorTable (Xbox #35 = PC #31)
	cmdListSlots[36] = cbListSetRootTable        // SetGraphicsRootDescriptorTable (Xbox #36 = PC #32)
	cmdListSlots[41] = cbListSetComputeRootCBV   // SetComputeRootConstantBufferView (Xbox #41 = PC #37)
	cmdListSlots[42] = cbListSetRootCBV          // SetGraphicsRootConstantBufferView (Xbox #42 = PC #38)
	cmdListSlots[43] = cbListSetComputeRootSRV   // SetComputeRootShaderResourceView (Xbox #43 = PC #39)
	cmdListSlots[44] = cbListSetRootSRV          // SetGraphicsRootShaderResourceView (Xbox #44 = PC #40)
	cmdListSlots[47] = cbListSetIB        // IASetIndexBuffer (Xbox #47 = PC #43)
	cmdListSlots[48] = cbListSetVB        // IASetVertexBuffers (Xbox #48 = PC #44)
	cmdListSlots[50] = cbListSetRTVs      // OMSetRenderTargets (Xbox #50 = PC #46)
	cmdListSlots[52] = cbListClearRTV     // ClearRenderTargetView (Xbox slot 52 = PC slot 48)
	cmdListSlots[60] = cbListCopyMem      // CopyMemoryToMemoryX (2020, 0x1E0)
	cmdListSlots[62] = cbListCopyMem      // CopyMemoryToMemoryX (2026 +2, 0x1F0)
	cmdListSlots[71] = cbListWrite32      // WriteValueBottomOfPipe 32-bit (2026 +2, 0x238)
	cmdListSlots[72] = cbListWrite64      // WriteValueBottomOfPipe 64-bit / Timestamp (2026 +2, 0x240)
	cmdListSlots[78] = cbListDispatch     // DispatchX (2020, 0x270)
	cmdListSlots[80] = cbListDispatch     // DispatchX (2026 +2, 0x280)
	cmdListSlots[81] = cbListDrawIdxX     // DrawIndexedX (0x288)
	cmdListSlots[148] = cbListAllocUpload // AllocateUploadMemory (0x4A0)
	cmdListProto := NewCOMObjectWithHeader("ID3D12GraphicsCommandListX", 160, 0x1000, cmdListSlots)
	cmdListVtable := *(*uintptr)(osPtr(cmdListProto))

	newCommandList := func() uintptr {
		st.listsCreated.Add(1)
		mem, _, _ := k32VirtualAlloc.Call(0, 0x1000, 0x3000, 0x04 /* PAGE_READWRITE */)
		if mem != 0 {
			*(*uintptr)(osPtr(mem)) = cmdListVtable
		}
		return mem
	}

	// Vtable partagée pour ID3D12FenceX : valeur 64-bit en mémoire directe à this+0x10 (slot 12 = GetAddress)
	fenceProto := NewCOMObjectWithHeader("ID3D12FenceX", 16, 0x20, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
		8: syscall.NewCallback(func(this uintptr) uintptr { // GetCompletedValue
			if this == 0 {
				return 0
			}
			return uintptr(atomic.LoadUint64((*uint64)(osPtr(this + 0x10))))
		}),
		9: syscall.NewCallback(func(this, val, ev uintptr) uintptr { // SetEventOnCompletion
			if this == 0 {
				return hresultInvalidArg
			}
			// Vérifie et enregistre le waiter sous st.mu pour éliminer la course lost-wakeup avec signalFence.
			st.mu.Lock()
			cur := atomic.LoadUint64((*uint64)(osPtr(this + 0x10)))
			if cur >= uint64(val) {
				st.mu.Unlock()
				if ev != 0 {
					_, _, _ = k32SetEvent.Call(ev)
				}
				return hresultOK
			}
			if ev != 0 {
				st.fenceWaiters[this] = append(st.fenceWaiters[this], fenceWaiter{val: uint64(val), event: ev})
				st.mu.Unlock()
				return hresultOK
			}
			st.mu.Unlock()
			// ev == NULL : attente synchrone D3D12 jusqu'à ce que le fence atteigne val
			for atomic.LoadUint64((*uint64)(osPtr(this+0x10))) < uint64(val) {
				runtime.Gosched()
			}
			return hresultOK
		}),
		10: syscall.NewCallback(func(this, v uintptr) uintptr { // Signal
			signalFence(this, uint64(v))
			return hresultOK
		}),
		11: cbOK, // GetCreationFlags
		12: syscall.NewCallback(func(this uintptr) uintptr { // GetGPUVirtualAddress / GetAddress (0x60)
			if this == 0 {
				return 0
			}
			return this + 0x10
		}),
	})
	fenceVtable := *(*uintptr)(osPtr(fenceProto))

	newFence := func(initVal uint64) uintptr {
		st.fencesCreated.Add(1)
		mem, _, _ := k32VirtualAlloc.Call(0, 0x40, 0x3000, 0x04)
		if mem != 0 {
			*(*uintptr)(osPtr(mem)) = fenceVtable
			atomic.StoreUint64((*uint64)(osPtr(mem+0x10)), initVal)
		}
		return mem
	}

	// Vtable partagée pour ID3D12DescriptorHeapX
	descHeapProto := NewCOMObject("ID3D12DescriptorHeapX", 16, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
		// Xbox ABI : slots 9 et 10 retournent le handle 8 octets directement dans RAX !
		9: syscall.NewCallback(func(this uintptr) uintptr { // GetCPUDescriptorHandleForHeapStart
			st.mu.Lock()
			b := st.heapBases[this]
			st.mu.Unlock()
			return uintptr(b)
		}),
		10: syscall.NewCallback(func(this uintptr) uintptr { // GetGPUDescriptorHandleForHeapStart
			st.mu.Lock()
			b := st.heapBases[this]
			st.mu.Unlock()
			return uintptr(b)
		}),
	})
	descHeapVtable := *(*uintptr)(osPtr(descHeapProto))

	newDescriptorHeap := func(descPtr uintptr) uintptr {
		st.heapsCreated.Add(1)
		// D3D12_DESCRIPTOR_HEAP_DESC : { Type u32, NumDescriptors u32, Flags u32, NodeMask u32 }
		// Suivi sur Xbox Series (GTA VI) d'un Placement optionnel { Address u64 (+16), Size u64 (+24) }.
		var baseAddr uintptr
		numDescs := uintptr(4096)
		stride := uintptr(128)
		var placementAddr uint64
		if descPtr != 0 {
			heapType := *(*uint32)(osPtr(descPtr))
			n := *(*uint32)(osPtr(descPtr + 4))
			if isGuestReadable(descPtr+16, 8) {
				placementAddr = *(*uint64)(osPtr(descPtr + 16))
			}
			if n > 0 && n <= 1_048_576 {
				numDescs = uintptr(n)
			}
			switch heapType {
			case 0:
				stride = 32
			case 1:
				stride = 16
			default:
				stride = 128
			}
		}
		bytes := numDescs * stride
		if bytes < 65536 {
			bytes = 65536
		}
		if placementAddr >= 0x10000 && placementAddr < 0x800000000000 && isGuestReadable(uintptr(placementAddr), uint64(bytes)) {
			baseAddr = uintptr(placementAddr)
		}
		if baseAddr == 0 {
			baseAddr, _, _ = k32VirtualAlloc.Call(0, bytes, 0x3000 /* COMMIT|RESERVE */, 0x04 /* PAGE_READWRITE */)
			if baseAddr == 0 {
				baseAddr = uintptr(st.nextHeapBase.Add(0x1000000))
			}
		}
		obj, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = descHeapVtable
			st.mu.Lock()
			st.heapBases[obj] = uint64(baseAddr)
			st.mu.Unlock()
		}
		return obj
	}

	// Vtable partagée pour ID3D12ResourceX (0 callback alloué par ressource)
	resProto := NewCOMObject("ID3D12ResourceX", 24, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
		8: syscall.NewCallback(func(this, subres, readRange, outDataPtr uintptr) uintptr { // Map
			st.mu.Lock()
			va := st.resGPUAddrs[this]
			st.mu.Unlock()
			if outDataPtr != 0 {
				*(*uintptr)(osPtr(outDataPtr)) = uintptr(va)
			}
			return hresultOK
		}),
		9: cbOK, // Unmap
		10: syscall.NewCallback(func(a0, a1 uintptr) uintptr { // GetDesc (struct return 56 bytes)
			st.mu.Lock()
			_, isA0 := st.resGPUAddrs[a0]
			var thisObj, outPtr uintptr
			if isA0 {
				thisObj, outPtr = a0, a1
			} else {
				thisObj, outPtr = a1, a0
			}
			d := st.resDescs[thisObj]
			st.mu.Unlock()
			if outPtr >= 0x10000 {
				*(*xgResourceDesc)(osPtr(outPtr)) = d
			}
			return outPtr
		}),
		11: syscall.NewCallback(func(this uintptr) uintptr { // GetGPUVirtualAddress
			st.mu.Lock()
			va := st.resGPUAddrs[this]
			st.mu.Unlock()
			return uintptr(va)
		}),
	})
	resVtable := *(*uintptr)(osPtr(resProto))

	newResource := func(gpuVA uint64, descPtr uintptr) uintptr {
		st.resCreated.Add(1)
		// D3D12_RESOURCE_DESC : { Dimension u32 (+0), Alignment u64 (+8), Width u64 (+16), Height u32 (+24), ... }
		var desc xgResourceDesc
		allocBytes := uintptr(4 << 20) // 4 MiB par défaut
		if descPtr != 0 {
			desc = *(*xgResourceDesc)(osPtr(descPtr))
			if desc.Dimension == 1 && desc.Width > 0 && desc.Width <= (512<<20) { // BUFFER
				allocBytes = uintptr((desc.Width + 4095) &^ 4095)
			} else if desc.Dimension == 3 && desc.Width > 0 && desc.Height > 0 { // TEXTURE2D
				s := desc.Width * uint64(desc.Height) * 4
				if s < 65536 {
					s = 65536
				}
				s = (s + 65535) &^ 65535
				if s <= (512 << 20) {
					allocBytes = uintptr(s)
				}
			}
		}
		if gpuVA == 0 {
			var mem uintptr
			if kbaseVirtAlloc2.Find() == nil {
				hProc, _, _ := k32GetCurrProc.Call()
				req := memAddressRequirements{
					LowestStartingAddress: 1 << 32,
					HighestEndingAddress:  (uintptr(1) << 40) - 1,
				}
				param := memExtendedParameter{
					Type:    1,
					Pointer: uintptr(unsafe.Pointer(&req)),
				}
				mem, _, _ = kbaseVirtAlloc2.Call(hProc, 0, allocBytes, 0x3000, 0x04, uintptr(unsafe.Pointer(&param)), 1)
			}
			if mem == 0 {
				mem, _, _ = k32VirtualAlloc.Call(0, allocBytes, 0x3000 /* COMMIT|RESERVE */, 0x04 /* PAGE_READWRITE */)
			}
			if mem != 0 {
				gpuVA = uint64(mem)
			} else {
				gpuVA = st.nextGPUAddr.Add(0x100000)
			}
		}
		obj, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = resVtable
			st.mu.Lock()
			st.resGPUAddrs[obj] = gpuVA
			st.resDescs[obj] = desc
			st.mu.Unlock()
		}
		return obj
	}

	genericChildProto := NewCOMObject("ID3D12DeviceChildX", 24, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
	})
	genericChildVtable := *(*uintptr)(osPtr(genericChildProto))
	newGenericChild := func(area string) uintptr {
		obj, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = genericChildVtable
		}
		return obj
	}

	psoProto := NewCOMObjectWithHeader("ID3D12PipelineStateX", 16, 0x40, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
		8: cbOK, // GetCachedBlob
	})
	psoVtable := *(*uintptr)(osPtr(psoProto))
	newPipelineState := func() uintptr {
		st.psoCreated.Add(1)
		// En-tête 0x40 octets mis à zéro (16 octets Object + 32 octets paquet inline Xbox)
		obj, _, _ := k32VirtualAlloc.Call(0, 0x40, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = psoVtable
		}
		return obj
	}

	// --- Callbacks des slots ID3D12DeviceX ---
	cbCreateQueue := syscall.NewCallback(func(this, desc, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateCommandQueue", "iid", FormatGUID(iid))
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newQueue()
		}
		return hresultOK
	})
	cbCreateAllocator := syscall.NewCallback(func(this, listType, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateCommandAllocator", "type", listType)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newAllocator()
		}
		return hresultOK
	})
	var psoTraceCount atomic.Int64
	inspectPSO := func(kind string, desc uintptr) {
		if !gpuTrace || desc < 0x10000 {
			return
		}
		n := psoTraceCount.Add(1)
		if n > 8 {
			return
		}
		rootSig := *(*uintptr)(osPtr(desc + 0x00))
		vsPtr := *(*uintptr)(osPtr(desc + 0x08))
		vsLen := *(*uint64)(osPtr(desc + 0x10))
		psPtr := *(*uintptr)(osPtr(desc + 0x18))
		psLen := *(*uint64)(osPtr(desc + 0x20))
		magicStr := func(p uintptr, l uint64) string {
			if p < 0x10000 || l < 4 {
				return "-"
			}
			b := unsafe.Slice((*byte)(osPtr(p)), 4)
			return fmt.Sprintf("%q(%02X%02X%02X%02X)", string(b), b[0], b[1], b[2], b[3])
		}
		slog.Info("GPU_TRACE "+kind, "n", n, "rootSig", fmt.Sprintf("0x%X", rootSig),
			"vsLen", vsLen, "vsMagic", magicStr(vsPtr, vsLen),
			"psLen", psLen, "psMagic", magicStr(psPtr, psLen))
	}
	storeDXBCBlob := func(p uintptr, l uint64) ([16]byte, bool) {
		var h [16]byte
		if p < 0x10000 || p >= 0x800000000000 || l < 32 || l > (16<<20) || !isGuestReadable(p, l) {
			return h, false
		}
		src := unsafe.Slice((*byte)(osPtr(p)), l)
		if src[0] != 'D' || src[1] != 'X' || src[2] != 'B' || src[3] != 'C' {
			return h, false
		}
		copy(h[:], src[4:20])
		st.mu.Lock()
		if _, exists := st.dxbcBlobs[h]; !exists {
			cp := make([]byte, l)
			copy(cp, src)
			st.dxbcBlobs[h] = cp
		}
		st.mu.Unlock()
		return h, true
	}
	parseRT0Blend := func(blendDescPtr uintptr) *gpu.HostBlendState {
		rt0 := blendDescPtr + 8
		en := *(*int32)(osPtr(rt0 + 0))
		logicEn := *(*int32)(osPtr(rt0 + 4))
		src := *(*uint32)(osPtr(rt0 + 8))
		dst := *(*uint32)(osPtr(rt0 + 12))
		op := *(*uint32)(osPtr(rt0 + 16))
		srcA := *(*uint32)(osPtr(rt0 + 20))
		dstA := *(*uint32)(osPtr(rt0 + 24))
		opA := *(*uint32)(osPtr(rt0 + 28))
		mask := *(*uint8)(osPtr(rt0 + 36))
		if (en != 0 && en != 1) || (logicEn != 0 && logicEn != 1) {
			return nil
		}
		if src < 1 || src > 19 || dst < 1 || dst > 19 || op < 1 || op > 5 ||
			srcA < 1 || srcA > 19 || dstA < 1 || dstA > 19 || opA < 1 || opA > 5 {
			return nil
		}
		if mask == 0 {
			mask = 0x0F
		}
		return &gpu.HostBlendState{
			BlendEnable:    en != 0,
			SrcBlend:       src,
			DestBlend:      dst,
			BlendOp:        op,
			SrcBlendAlpha:  srcA,
			DestBlendAlpha: dstA,
			BlendOpAlpha:   opA,
			WriteMask:      mask,
		}
	}
	parseInputLayout := func(elemsPtr uintptr, num uint32) []gpu.HostInputElement {
		if elemsPtr < 0x10000 || elemsPtr >= 0x800000000000 || num == 0 || num > 16 {
			return nil
		}
		if !isGuestReadable(elemsPtr, uint64(num)*32) {
			return nil
		}
		out := make([]gpu.HostInputElement, 0, num)
		for i := uint32(0); i < num; i++ {
			e := elemsPtr + uintptr(i)*32
			namePtr := *(*uintptr)(osPtr(e + 0))
			if namePtr < 0x10000 || namePtr >= 0x800000000000 || !isGuestReadable(namePtr, 1) {
				return nil
			}
			var nb []byte
			for k := uintptr(0); k < 64; k++ {
				ch := *(*byte)(osPtr(namePtr + k))
				if ch == 0 {
					break
				}
				if ch < 0x20 || ch > 0x7E {
					return nil
				}
				nb = append(nb, ch)
			}
			if len(nb) == 0 {
				return nil
			}
			semIdx := *(*uint32)(osPtr(e + 8))
			fmtID := *(*uint32)(osPtr(e + 12))
			slot := *(*uint32)(osPtr(e + 16))
			off := *(*uint32)(osPtr(e + 20))
			if fmtID == 0 || fmtID > 132 || slot > 15 {
				return nil
			}
			out = append(out, gpu.HostInputElement{
				SemanticName:      string(nb),
				SemanticIndex:     semIdx,
				Format:            fmtID,
				InputSlot:         slot,
				AlignedByteOffset: off,
				HasOffset:         true,
			})
		}
		return out
	}
	storeGraphicsPSODesc := func(psoObj, desc uintptr) {
		if desc < 0x10000 {
			return
		}
		vsPtr := *(*uintptr)(osPtr(desc + 0x08))
		vsLen := *(*uint64)(osPtr(desc + 0x10))
		psPtr := *(*uintptr)(osPtr(desc + 0x18))
		psLen := *(*uint64)(osPtr(desc + 0x20))
		ilPtr := *(*uintptr)(osPtr(desc + 0x220))
		ilNum := *(*uint32)(osPtr(desc + 0x228))
		gp := &guestPSO{
			blend:      parseRT0Blend(desc + 0x78),
			inputElems: parseInputLayout(ilPtr, ilNum),
		}
		if h, ok := storeDXBCBlob(vsPtr, vsLen); ok {
			gp.vsHash, gp.hasVS = h, true
		}
		if h, ok := storeDXBCBlob(psPtr, psLen); ok {
			gp.psHash, gp.hasPS = h, true
		}
		if gp.hasVS || gp.hasPS {
			st.mu.Lock()
			st.psos[psoObj] = gp
			st.mu.Unlock()
		}
	}
	storeComputePSODesc := func(psoObj, desc uintptr) {
		if desc < 0x10000 {
			return
		}
		csPtr := *(*uintptr)(osPtr(desc + 0x08))
		csLen := *(*uint64)(osPtr(desc + 0x10))
		if h, ok := storeDXBCBlob(csPtr, csLen); ok {
			st.mu.Lock()
			st.psos[psoObj] = &guestPSO{csHash: h, hasCS: true}
			st.mu.Unlock()
		}
	}
	cbCreatePSO := syscall.NewCallback(func(this, desc, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateGraphicsPSO")
		psoObj := newPipelineState()
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = psoObj
		}
		storeGraphicsPSODesc(psoObj, desc)
		inspectPSO("CreatePSO", desc)
		return hresultOK
	})
	cbCreateComputePSO := syscall.NewCallback(func(this, desc, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateComputePSO")
		psoObj := newPipelineState()
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = psoObj
		}
		storeComputePSODesc(psoObj, desc)
		return hresultOK
	})
	cbCreatePSOStream := syscall.NewCallback(func(this, streamDesc, iid, outPtr uintptr) uintptr {
		psoObj := newPipelineState()
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = psoObj
		}
		if streamDesc >= 0x10000 {
			sz := *(*uint64)(osPtr(streamDesc + 0))
			sub := *(*uintptr)(osPtr(streamDesc + 8))
			if sub >= 0x10000 && sz >= 24 && sz <= 4096 && isGuestReadable(sub, sz) {
				q := unsafe.Slice((*uint64)(osPtr(sub)), sz/8)
				gp := &guestPSO{}
				// Un subobject shader dans D3D12_PIPELINE_STATE_STREAM_DESC est précédé de son tag u32
				// (1=VS, 2=PS, 6=CS) aligné sur 8 octets, suivi de { pShaderBytecode u64, BytecodeLength u64 }.
				for i := 1; i+1 < len(q); i++ {
					tag := uint32(q[i-1])
					if tag == 8 && gp.blend == nil {
						offBytes := uintptr(i-1)*8 + 4
						if offBytes+48 <= uintptr(sz) {
							gp.blend = parseRT0Blend(sub + offBytes)
						}
						continue
					}
					if tag == 12 && len(gp.inputElems) == 0 {
						gp.inputElems = parseInputLayout(uintptr(q[i]), uint32(q[i+1]))
						continue
					}
					if tag != 1 && tag != 2 && tag != 6 {
						continue
					}
					ptrVal := q[i]
					lenVal := q[i+1]
					if ptrVal >= 0x10000 && ptrVal < 0x800000000000 && (ptrVal&3) == 0 && lenVal >= 32 && lenVal <= (16<<20) {
						if h, ok := storeDXBCBlob(uintptr(ptrVal), lenVal); ok {
							if tag == 1 && !gp.hasVS {
								gp.vsHash, gp.hasVS = h, true
							} else if tag == 2 && !gp.hasPS {
								gp.psHash, gp.hasPS = h, true
							} else if tag == 6 && !gp.hasCS {
								gp.csHash, gp.hasCS = h, true
							}
						}
					}
				}
				if gp.hasVS || gp.hasPS || gp.hasCS {
					st.mu.Lock()
					st.psos[psoObj] = gp
					st.mu.Unlock()
				}
			}
		}
		return hresultOK
	})
	cbDeserializePSOX := syscall.NewCallback(func(this, state, iid, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = 0
		}
		return hresultNotImpl
	})
	cbSerializePSOX := syscall.NewCallback(func(this, pso uintptr) uintptr {
		return 0x80070005 // E_ACCESSDENIED
	})
	cbCreateDerivedPSOX := syscall.NewCallback(func(this, base, count, patches, iid, outPtr uintptr) uintptr {
		psoObj := newPipelineState()
		if base != 0 {
			st.mu.Lock()
			if bp, ok := st.psos[base]; ok && bp != nil {
				st.psos[psoObj] = &guestPSO{
					vsHash: bp.vsHash, psHash: bp.psHash, csHash: bp.csHash,
					hasVS: bp.hasVS, hasPS: bp.hasPS, hasCS: bp.hasCS,
				}
			}
			st.mu.Unlock()
		}
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = psoObj
		}
		return hresultOK
	})
	cbCreateGraphicsPSOX := syscall.NewCallback(func(this, desc, extensions, extended, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateGraphicsPipelineStateX", "ext", extensions)
		psoObj := newPipelineState()
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = psoObj
		}
		storeGraphicsPSODesc(psoObj, desc)
		inspectPSO("CreateGraphicsPSOX", desc)
		return hresultOK
	})
	cbCreateComputePSOX := syscall.NewCallback(func(this, desc, extensions, extended, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateComputePipelineStateX", "ext", extensions)
		psoObj := newPipelineState()
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = psoObj
		}
		storeComputePSODesc(psoObj, desc)
		return hresultOK
	})
	cbCreateList := syscall.NewCallback(func(this, nodeMask, listType, alloc, pso, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateCommandList", "type", listType)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newCommandList()
		}
		return hresultOK
	})
	cbCreateListX := syscall.NewCallback(func(this, nodeMask, listType, flags, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateCommandListX", "type", listType, "flags", flags)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newCommandList()
		}
		return hresultOK
	})
	cbCreateDescHeap := syscall.NewCallback(func(this, desc, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateDescriptorHeap")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newDescriptorHeap(desc)
		}
		return hresultOK
	})
	cbDescIncrSize := syscall.NewCallback(func(this, heapType uintptr) uintptr {
		switch uint32(heapType) {
		case 0: // CBV_SRV_UAV
			return 32
		case 1: // SAMPLER
			return 16
		default: // RTV / DSV
			return 128
		}
	})
	cbCreateRootSig := syscall.NewCallback(func(this, nodeMask, blob, blobLen, iid, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newGenericChild("ID3D12RootSignatureX")
		}
		return hresultOK
	})
	cbCustomHeapProps := syscall.NewCallback(func(a0, a1, a2, a3 uintptr) uintptr {
		// Quirk ABI Xbox : struct retournée via pointeur caché (20 octets = 5 u32)
		// On détecte si a0 est le pointeur de retour (a0 != deviceObj) ou a1.
		outPtr := a1
		if a0 != st.deviceObj && a0 != 0 {
			outPtr = a0
		}
		if outPtr != 0 {
			words := unsafe.Slice((*uint32)(osPtr(outPtr)), 5)
			words[0] = 4 // D3D12_HEAP_TYPE_CUSTOM
			words[1] = 2 // CPU_PAGE_PROPERTY_WRITE_COMBINE
			words[2] = 1 // MEMORY_POOL_L0
			words[3] = 1 // CreationNodeMask
			words[4] = 1 // VisibleNodeMask
		}
		return outPtr
	})
	calcDescSize := func(d uintptr) uint64 {
		if d == 0 {
			return 65536
		}
		dim := *(*uint32)(osPtr(d))
		w := *(*uint64)(osPtr(d + 16))
		h := uint64(*(*uint32)(osPtr(d + 24)))
		fmtID := *(*uint32)(osPtr(d + 32))
		if dim == 1 && w > 0 {
			return (w + 65535) &^ 65535
		}
		if w > 0 && h > 0 {
			bpe, block := xgFormatMetrics(fmtID)
			bw := (w + uint64(block) - 1) / uint64(block)
			bh := (h + uint64(block) - 1) / uint64(block)
			s := bw * bh * uint64(bpe)
			if s < 65536 {
				s = 65536
			}
			return (s + 65535) &^ 65535
		}
		return 65536
	}
	cbAllocInfo := syscall.NewCallback(func(a0, a1, a2, a3, a4 uintptr) uintptr {
		outPtr := a1
		descPtr := a4
		if a0 != st.deviceObj && a0 != 0 {
			outPtr = a0
			descPtr = a4
		}
		size := calcDescSize(descPtr)
		slog.Debug("D3D12DeviceX!GetResourceAllocationInfo", "size", size)
		if outPtr != 0 {
			words := unsafe.Slice((*uint64)(osPtr(outPtr)), 2)
			words[0] = size  // SizeInBytes
			words[1] = 65536 // Alignment
		}
		return outPtr
	})
	cbAllocInfo2 := syscall.NewCallback(func(outPtr, this, visible, countArg, descsPtr, eachPtr uintptr) uintptr {
		count := uintptr(uint32(countArg))
		var total uint64
		for i := uintptr(0); i < count; i++ {
			// D3D12_RESOURCE_DESC1 fait 72 octets (56 + SamplerFeedbackMipRegion)
			size := uint64(65536)
			if descsPtr != 0 {
				size = calcDescSize(descsPtr + i*72)
			}
			if eachPtr != 0 {
				e := unsafe.Slice((*uint64)(osPtr(eachPtr+i*24)), 3)
				e[0] = total // Offset
				e[1] = 65536 // Alignment
				e[2] = size  // SizeInBytes
			}
			total += size
		}
		if total == 0 {
			total = 65536
		}
		slog.Debug("D3D12DeviceX!GetResourceAllocationInfo2", "count", count, "total", total)
		if outPtr != 0 {
			words := unsafe.Slice((*uint64)(osPtr(outPtr)), 2)
			words[0] = total
			words[1] = 65536
		}
		return outPtr
	})
	cbCopyableFootprints := syscall.NewCallback(func(this, descPtr, firstSub, numSub, baseOffset, layoutsPtr, numRowsPtr, rowSizePtr, totalBytesPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!GetCopyableFootprints", "numSub", numSub)
		w := uint64(256)
		h := uint32(1)
		fmtID := uint32(0)
		if descPtr != 0 {
			if ww := *(*uint64)(osPtr(descPtr + 16)); ww > 0 {
				w = ww
			}
			if hh := *(*uint32)(osPtr(descPtr + 24)); hh > 0 {
				h = hh
			}
			fmtID = *(*uint32)(osPtr(descPtr + 32))
		}
		rowPitch := uint32((w*4 + 255) &^ 255)
		if rowPitch < 256 {
			rowPitch = 256
		}
		offset := uint64(baseOffset)
		for i := uintptr(0); i < numSub; i++ {
			if layoutsPtr != 0 {
				// D3D12_PLACED_SUBRESOURCE_FOOTPRINT (32 bytes): Offset u64, Format u32, Width u32, Height u32, Depth u32, RowPitch u32
				base := layoutsPtr + i*32
				*(*uint64)(osPtr(base + 0)) = offset
				*(*uint32)(osPtr(base + 8)) = fmtID
				*(*uint32)(osPtr(base + 12)) = uint32(w)
				*(*uint32)(osPtr(base + 16)) = h
				*(*uint32)(osPtr(base + 20)) = 1
				*(*uint32)(osPtr(base + 24)) = rowPitch
			}
			if numRowsPtr != 0 {
				*(*uint32)(osPtr(numRowsPtr + i*4)) = h
			}
			if rowSizePtr != 0 {
				*(*uint64)(osPtr(rowSizePtr + i*8)) = uint64(rowPitch)
			}
			offset += uint64(rowPitch) * uint64(h)
			offset = (offset + 511) &^ 511
		}
		if totalBytesPtr != 0 {
			*(*uint64)(osPtr(totalBytesPtr)) = offset - uint64(baseOffset)
		}
		return 0
	})
	allocCommittedVA := func(heapProps, descPtr uintptr) uint64 {
		heapKind := uint32(1)
		if heapProps != 0 {
			heapKind = *(*uint32)(osPtr(heapProps))
		}
		size := uint64(65536)
		if descPtr != 0 {
			dim := *(*uint32)(osPtr(descPtr))
			w := *(*uint64)(osPtr(descPtr + 16))
			h := uint64(*(*uint32)(osPtr(descPtr + 24)))
			if dim == 1 && w > 0 {
				size = (w + 65535) &^ 65535
			} else if w > 0 && h > 0 {
				s := w * h * 4
				if s < 65536 {
					s = 65536
				}
				size = (s + 65535) &^ 65535
			}
		}
		return xmemAllocCommitted(size, heapKind)
	}
	cbCreateCommitted := syscall.NewCallback(func(this, heapProps, heapFlags, desc, state, clearVal, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateCommittedResource")
		va := allocCommittedVA(heapProps, desc)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newResource(va, desc)
		}
		return hresultOK
	})
	cbCreateCommitted3 := syscall.NewCallback(func(this, heapProps, heapFlags, desc, layout, clearVal, protected, castCount, castFmts, iid, outPtr uintptr) uintptr {
		var dim, h, fmtID uint32
		var w uint64
		if desc != 0 {
			dim = *(*uint32)(osPtr(desc))
			w = *(*uint64)(osPtr(desc + 16))
			h = *(*uint32)(osPtr(desc + 24))
			fmtID = *(*uint32)(osPtr(desc + 32))
		}
		va := allocCommittedVA(heapProps, desc)
		res := newResource(va, desc)
		slog.Info("D3D12DeviceX!CreateCommittedResource3", "dim", dim, "w", w, "h", h, "fmt", fmtID, "va", fmt.Sprintf("0x%X", va), "res", fmt.Sprintf("0x%X", res))
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = res
		}
		return hresultOK
	})
	cbCreatePlaced := syscall.NewCallback(func(this, heap, offset, desc, state, clearVal, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreatePlacedResource")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newResource(0, desc)
		}
		return hresultOK
	})
	cbCreatePlacedAtAddr := syscall.NewCallback(func(this, gpuAddr, desc, state, clearVal, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreatePlacedResourceX", "gpuAddr", gpuAddr)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newResource(uint64(gpuAddr), desc)
		}
		return hresultOK
	})
	cbCreateFence := syscall.NewCallback(func(this, initVal, flags, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateFence", "init", initVal)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newFence(uint64(initVal))
		}
		return hresultOK
	})
	cbCreateQueryHeap := syscall.NewCallback(func(this, desc, iid, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newGenericChild("ID3D12QueryHeapX")
		}
		return hresultOK
	})
	cbCreateCmdSig := syscall.NewCallback(func(this, desc, rootSig, iid, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newGenericChild("ID3D12CommandSignatureX")
		}
		return hresultOK
	})
	cbCreateHeap := syscall.NewCallback(func(this, desc, iid, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newGenericChild("ID3D12HeapX")
		}
		return hresultOK
	})

	cbCheckFeatureSupport := syscall.NewCallback(func(this, featureArg, dataPtr, dataSizeArg uintptr) uintptr {
		feature := uint32(featureArg)
		dataSize := uint32(dataSizeArg)
		slog.Debug("D3D12DeviceX!CheckFeatureSupport", "feature", feature, "size", dataSize)
		if dataPtr == 0 {
			return hresultInvalidArg
		}
		switch feature {
		case 0: // D3D12_FEATURE_D3D12_OPTIONS (64 bytes = 16 u32)
			if dataSize == 64 {
				opts := [16]uint32{
					1, 1, 2, 3, 3, 1, 1, 1,
					3, 40, 0, 0, 0, 0, 1, 2,
				}
				dst := unsafe.Slice((*uint32)(osPtr(dataPtr)), 16)
				copy(dst, opts[:])
				return hresultOK
			}
		case 1: // D3D12_FEATURE_ARCHITECTURE (16 bytes)
			if dataSize == 16 {
				a := unsafe.Slice((*uint32)(osPtr(dataPtr)), 4)
				a[1], a[2], a[3] = 0, 1, 1 // UMA, CacheCoherentUMA
				return hresultOK
			}
		case 2: // D3D12_FEATURE_FEATURE_LEVELS (24 bytes)
			if dataSize == 24 {
				// { NumFeatureLevels u32, _pad u32, pFeatureLevelsRequested ptr, MaxSupportedFeatureLevel u32 }
				*(*uint32)(osPtr(dataPtr + 16)) = 0xC200 // D3D_FEATURE_LEVEL_12_2
				return hresultOK
			}
		case 3: // D3D12_FEATURE_FORMAT_SUPPORT (12 bytes)
			if dataSize == 12 {
				q := unsafe.Slice((*uint32)(osPtr(dataPtr)), 3)
				q[1] = 0x7FFFFFFF
				q[2] = 0xFFFF
				return hresultOK
			}
		case 4: // D3D12_FEATURE_MULTISAMPLE_QUALITY_LEVELS (16 bytes)
			if dataSize == 16 {
				q := unsafe.Slice((*uint32)(osPtr(dataPtr)), 4)
				if q[1] == 1 || q[1] == 2 || q[1] == 4 || q[1] == 8 {
					q[3] = 1
				} else {
					q[3] = 0
				}
				return hresultOK
			}
		default:
			slog.Warn("D3D12DeviceX!CheckFeatureSupport non implémenté", "feature", feature, "size", dataSize)
		}
		return hresultOK
	})

	cbCreateShaderCacheSession := syscall.NewCallback(func(this, desc, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateShaderCacheSession")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newGenericChild("ID3D12ShaderCacheSession")
		}
		return hresultOK
	})

	var nextFrameToken atomic.Uint64
	nextFrameToken.Store(1)

	var (
		winmmDLL            = syscall.NewLazyDLL("winmm.dll")
		procTimeBeginPeriod = winmmDLL.NewProc("timeBeginPeriod")
		framePacerMu        sync.Mutex
		nextFrameDeadline   time.Time
		framePeriod         = 16667 * time.Microsecond // 60 Hz par défaut
	)
	if procTimeBeginPeriod.Find() == nil {
		_, _, _ = procTimeBeginPeriod.Call(1) // résolution timer Windows 1 ms (indispensable pour le streaming audio Bink2/Opus)
	}
	cbSetFrameInterval := syscall.NewCallback(func(this, output, interval, period, flags uintptr) uintptr {
		p := uint32(period)
		framePacerMu.Lock()
		if p >= 4000 && p <= 50000 {
			framePeriod = time.Duration(p) * time.Microsecond
		}
		framePacerMu.Unlock()
		slog.Debug("D3D12DeviceX!SetFrameIntervalX", "interval", interval, "period", p)
		return hresultOK
	})
	cbScheduleFrameEvent := syscall.NewCallback(func(this, eventType, offset, flags uintptr) uintptr {
		slog.Debug("D3D12DeviceX!ScheduleFrameEventX", "event", eventType)
		return hresultOK
	})
	cbWaitFrameEvent := syscall.NewCallback(func(this, eventType, timeout, reserved, flags, outToken uintptr) uintptr {
		now := time.Now()
		framePacerMu.Lock()
		period := framePeriod
		if nextFrameDeadline.IsZero() || now.After(nextFrameDeadline.Add(period*2)) {
			nextFrameDeadline = now.Add(period)
		} else {
			nextFrameDeadline = nextFrameDeadline.Add(period)
		}
		wait := time.Until(nextFrameDeadline)
		framePacerMu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}
		tok := nextFrameToken.Add(1)
		slog.Debug("D3D12DeviceX!WaitFrameEventX", "event", eventType, "token", tok)
		if outToken != 0 {
			*(*uint64)(osPtr(outToken)) = tok
		}
		return hresultOK
	})
	cbGetFrameStats := syscall.NewCallback(func(this, token, ty, countPtr, outStats uintptr) uintptr {
		slog.Debug("D3D12DeviceX!GetFrameStatisticsX", "token", token, "ty", uint32(ty))
		if countPtr >= 0x10000 {
			*(*uint32)(osPtr(countPtr)) = 1
		}
		if outStats >= 0x10000 {
			b := unsafe.Slice((*byte)(osPtr(outStats)), 64)
			clear(b)
			*(*uint32)(osPtr(outStats + 0)) = uint32(ty)
			now := uint64(time.Now().UnixNano() / 100)
			*(*uint64)(osPtr(outStats + 0x10)) = now
			*(*uint64)(osPtr(outStats + 0x18)) = now
		}
		return hresultOK
	})

	logOK := func(name string) uintptr {
		return syscall.NewCallback(func() uintptr {
			slog.Debug("D3D12DeviceX!" + name)
			return hresultOK
		})
	}

	cbCreateRTV := syscall.NewCallback(func(this, res, descPtr, handle uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateRTV", "res", fmt.Sprintf("0x%X", res), "handle", fmt.Sprintf("0x%X", handle))
		if handle >= 0x10000 {
			*(*uint32)(osPtr(handle + 0)) = 0x52545631 // "RTV1"
			*(*uint32)(osPtr(handle + 4)) = 1
			*(*uintptr)(osPtr(handle + 8)) = res
			if descPtr >= 0x10000 {
				copy(unsafe.Slice((*byte)(osPtr(handle+16)), 24), unsafe.Slice((*byte)(osPtr(descPtr)), 24))
			}
			st.mu.Lock()
			st.rtvResources[handle] = res
			st.mu.Unlock()
		}
		return 0
	})
	cbCreateDSV := syscall.NewCallback(func(this, res, descPtr, handle uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateDSV", "res", fmt.Sprintf("0x%X", res), "handle", fmt.Sprintf("0x%X", handle))
		if handle >= 0x10000 {
			*(*uint32)(osPtr(handle + 0)) = 0x44535631 // "DSV1"
			*(*uint32)(osPtr(handle + 4)) = 1
			*(*uintptr)(osPtr(handle + 8)) = res
			if descPtr >= 0x10000 {
				copy(unsafe.Slice((*byte)(osPtr(handle+16)), 24), unsafe.Slice((*byte)(osPtr(descPtr)), 24))
			}
		}
		return 0
	})
	writeSRV2 := func(handle uintptr, gpuVA uint64, w, h, fmtID, dim, swz uint32) {
		if handle < 0x10000 {
			return
		}
		*(*uint32)(osPtr(handle + 0)) = 0x53525632 // "SRV2"
		*(*uint32)(osPtr(handle + 4)) = fmtID
		*(*uint64)(osPtr(handle + 8)) = gpuVA
		*(*uint32)(osPtr(handle + 16)) = w
		*(*uint32)(osPtr(handle + 20)) = h
		*(*uint32)(osPtr(handle + 24)) = dim
		*(*uint32)(osPtr(handle + 28)) = swz
	}
	var srvCount, placedSRVCount atomic.Uint64
	cbCreateSRV := syscall.NewCallback(func(this, res, descPtr, handle uintptr) uintptr {
		srvCount.Add(1)
		var va uint64
		var d xgResourceDesc
		if res != 0 {
			st.mu.Lock()
			va = st.resGPUAddrs[res]
			d = st.resDescs[res]
			st.mu.Unlock()
		}
		fmtID := d.Format
		if descPtr >= 0x10000 {
			if vf := *(*uint32)(osPtr(descPtr)); vf != 0 {
				fmtID = vf
			}
		}
		writeSRV2(handle, va, uint32(d.Width), d.Height, fmtID, d.Dimension, d.Layout)
		return 0
	})
	cbCreatePlacedSRV := syscall.NewCallback(func(this, resDescPtr, srvDescPtr, handle uintptr) uintptr {
		n := placedSRVCount.Add(1)
		var d xgResourceDesc
		if resDescPtr >= 0x10000 {
			d = *(*xgResourceDesc)(osPtr(resDescPtr))
		}
		var va uint64
		if srvDescPtr >= 0x10000 {
			// Sur Xbox Series (CreatePlacedShaderResourceView, slot 70 / 0x230), l'adresse GPU 64-bit
			// est stockée dans srvDescPtr + 40 (desc[10..11]), juste après les 40 octets D3D12_SHADER_RESOURCE_VIEW_DESC.
			va = *(*uint64)(osPtr(srvDescPtr + 40))
		}
		if gpuTrace && n <= 6 && d.Dimension == 3 {
			slog.Info("GPU_TRACE CreatePlacedSRV(Tex2D)", "n", n, "va", fmt.Sprintf("0x%X", va), "w", d.Width, "h", d.Height, "fmt", d.Format, "layout", fmt.Sprintf("0x%X", d.Layout))
		}
		writeSRV2(handle, va, uint32(d.Width), d.Height, d.Format, d.Dimension, d.Layout)
		return 0
	})
	cbCreateUAV := syscall.NewCallback(func(this, res, counter, descPtr, handle uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateUAV", "res", fmt.Sprintf("0x%X", res), "handle", fmt.Sprintf("0x%X", handle))
		if handle >= 0x10000 {
			*(*uint32)(osPtr(handle + 0)) = 0x55415631 // "UAV1"
			*(*uint32)(osPtr(handle + 4)) = 1
			*(*uintptr)(osPtr(handle + 8)) = res
			*(*uintptr)(osPtr(handle + 16)) = counter
		}
		return 0
	})
	cbCreateCBV := syscall.NewCallback(func(this, descPtr, handle uintptr) uintptr {
		if handle >= 0x10000 {
			*(*uint32)(osPtr(handle + 0)) = 0x43425631 // "CBV1"
			if descPtr >= 0x10000 {
				copy(unsafe.Slice((*byte)(osPtr(handle+8)), 16), unsafe.Slice((*byte)(osPtr(descPtr)), 16))
			}
		}
		return 0
	})
	var nextPagePool atomic.Uint64
	cbRegisterPagePool := syscall.NewCallback(func(this, addr, pages, outHandle uintptr) uintptr {
		if outHandle == 0 || uint32(pages) == 0 {
			return hresultInvalidArg
		}
		id := nextPagePool.Add(1)
		*(*uintptr)(osPtr(outHandle)) = uintptr(id)
		slog.Debug("D3D12DeviceX!RegisterPagePool", "addr", fmt.Sprintf("0x%X", addr), "pages", uint32(pages), "handle", id)
		return hresultOK
	})
	cbCopyDescs := syscall.NewCallback(func(this, numDstArg, dstStarts, dstSizes, numSrcArg, srcStarts, srcSizes, heapType uintptr) uintptr {
		numDst := int(uint32(numDstArg))
		numSrc := int(uint32(numSrcArg))
		if numDst == 0 || numSrc == 0 || dstStarts == 0 || srcStarts == 0 {
			return 0
		}
		stride := uintptr(128)
		switch uint32(heapType) {
		case 0:
			stride = 32
		case 1:
			stride = 16
		}
		dStarts := unsafe.Slice((*uintptr)(osPtr(dstStarts)), numDst)
		sStarts := unsafe.Slice((*uintptr)(osPtr(srcStarts)), numSrc)
		var dSizes, sSizes []uint32
		if dstSizes != 0 {
			dSizes = unsafe.Slice((*uint32)(osPtr(dstSizes)), numDst)
		}
		if srcSizes != 0 {
			sSizes = unsafe.Slice((*uint32)(osPtr(srcSizes)), numSrc)
		}
		di, doff := 0, uintptr(0)
		si, soff := 0, uintptr(0)
		for di < numDst && si < numSrc {
			dn := uintptr(1)
			if dSizes != nil {
				dn = uintptr(dSizes[di])
			}
			sn := uintptr(1)
			if sSizes != nil {
				sn = uintptr(sSizes[si])
			}
			dRem := dn - doff
			sRem := sn - soff
			if dRem == 0 {
				di++
				doff = 0
				continue
			}
			if sRem == 0 {
				si++
				soff = 0
				continue
			}
			chunk := dRem
			if sRem < chunk {
				chunk = sRem
			}
			if dStarts[di] >= 0x10000 && sStarts[si] >= 0x10000 {
				copy(
					unsafe.Slice((*byte)(osPtr(dStarts[di]+doff*stride)), chunk*stride),
					unsafe.Slice((*byte)(osPtr(sStarts[si]+soff*stride)), chunk*stride),
				)
			}
			doff += chunk
			soff += chunk
		}
		return 0
	})
	cbCopyDescSimple := syscall.NewCallback(func(this, num, dstStart, srcStart, heapType uintptr) uintptr {
		cnt := uintptr(uint32(num))
		if cnt == 0 || dstStart < 0x10000 || srcStart < 0x10000 {
			return 0
		}
		stride := uintptr(128)
		switch uint32(heapType) {
		case 0:
			stride = 32
		case 1:
			stride = 16
		}
		n := cnt * stride
		copy(unsafe.Slice((*byte)(osPtr(dstStart)), n), unsafe.Slice((*byte)(osPtr(srcStart)), n))
		return 0
	})

	// Allocation de ID3D12DeviceX avec en-tête 0x58 octets (VIctory device.rs)
	// et octet +0x18 = 1 (out_of_line[0] = 1 pour forcer le passage par la vtable).
	st.deviceObj = NewCOMObjectWithHeader("ID3D12DeviceX", 200, 0x58, map[int]uintptr{
		0:   cbDeviceQI,
		3:   cbOK, // GetPrivateData
		4:   cbOK, // SetPrivateData
		5:   cbOK, // SetPrivateDataInterface
		6:   cbOK, // SetName
		7:   cbOne,
		8:   cbCreateQueue,
		9:   cbCreateAllocator,
		10:  cbCreatePSO,
		11:  cbCreateComputePSO,
		12:  cbCreateList,
		13:  cbCheckFeatureSupport,
		14:  cbCreateDescHeap,
		15:  cbDescIncrSize,
		16:  cbCreateRootSig,
		17:  cbCreateCBV,
		18:  cbCreateSRV,
		19:  cbCreateUAV,
		20:  cbCreateRTV,
		21:  cbCreateDSV,
		22:  logOK("CreateSampler"),
		23:  cbCopyDescs,
		24:  cbCopyDescSimple,
		25:  cbAllocInfo,
		26:  cbCustomHeapProps,
		27:  cbCreateCommitted,
		28:  cbCreateHeap,
		29:  cbCreatePlaced,
		36:  cbCreateFence,
		38:  cbCopyableFootprints,
		39:  cbCreateQueryHeap,
		41:  cbCreateCmdSig,
		48:  cbCreateQueue,        // CreateCommandQueueX
		52:  cbOK,                 // SetDebugFlagsX
		55:  cbCreateDerivedPSOX,  // CreateDerivedPipelineStateX (6 args)
		56:  cbSerializePSOX,      // SerializeGraphicsPipelineStateX
		57:  cbDeserializePSOX,    // DeserializeGraphicsPipelineStateX
		60:  cbCreateGraphicsPSOX, // CreateGraphicsPipelineStateX (6 args)
		61:  logOK("CreateSampler2"),
		62:  cbCreateComputePSOX, // CreateComputePipelineStateX (6 args)
		66:  cbSerializePSOX,     // SerializeComputePipelineStateX
		67:  cbDeserializePSOX,   // DeserializeComputePipelineStateX
		69:  cbOK,                // Debug filter
		70:  cbCreatePlacedSRV,
		71:  logOK("CreatePlacedUAV"),
		73:  cbCreateListX, // CreateCommandListX
		74:  cbRegisterPagePool,
		75:  logOK("UnregisterPagePool"),
		76:  cbCreateCommitted,
		78:  cbSetFrameInterval,
		79:  cbScheduleFrameEvent,
		80:  cbWaitFrameEvent,
		81:  cbGetFrameStats,
		86:  cbCreatePSOStream,
		108: cbAllocInfo2,
		118: cbCreateShaderCacheSession,
		127: cbCreateCommitted3,
		133: cbCreatePlacedAtAddr,
	})

	// Active le drapeau out_of_line à +0x18 (vérifié par VIctory)
	if st.deviceObj != 0 {
		*(*uint32)(osPtr(st.deviceObj + 0x18)) = 1
	}

	slog.Debug("ID3D12DeviceX initialisé (en-tête 0x58, out_of_line=1, 160 slots)")
	return st.deviceObj
}
