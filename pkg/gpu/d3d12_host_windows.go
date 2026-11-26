//go:build windows

package gpu

import (
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	modD3D12Host           = syscall.NewLazyDLL("d3d12.dll")
	modDXGIHost            = syscall.NewLazyDLL("dxgi.dll")
	modK32Host             = syscall.NewLazyDLL("kernel32.dll")
	procD3D12CreateDevice  = modD3D12Host.NewProc("D3D12CreateDevice")
	procD3D12GetDebugIface = modD3D12Host.NewProc("D3D12GetDebugInterface")
	procCreateDXGIFactory2 = modDXGIHost.NewProc("CreateDXGIFactory2")
	procCreateEventW       = modK32Host.NewProc("CreateEventW")
	procWaitForSingleObj   = modK32Host.NewProc("WaitForSingleObject")

	// IID_ID3D12Debug = {344488b7-6846-474b-b989-f027448245e0}
	guidID3D12Debug = [16]byte{
		0xb7, 0x88, 0x44, 0x34, 0x46, 0x68, 0x4b, 0x47,
		0xb9, 0x89, 0xf0, 0x27, 0x44, 0x82, 0x45, 0xe0,
	}
	// IID_ID3D12InfoQueue = {0742a90b-c387-483f-b946-30a7e4e61458}
	guidID3D12InfoQueue = [16]byte{
		0x0b, 0xa9, 0x42, 0x07, 0x87, 0xc3, 0x3f, 0x48,
		0xb9, 0x46, 0x30, 0xa7, 0xe4, 0xe6, 0x14, 0x58,
	}

	// IID_ID3D12Device = {189819f1-1db6-4b57-be54-1821339b85f7}
	guidID3D12Device = [16]byte{
		0xf1, 0x19, 0x98, 0x18, 0xb6, 0x1d, 0x57, 0x4b,
		0xbe, 0x54, 0x18, 0x21, 0x33, 0x9b, 0x85, 0xf7,
	}
	// IID_ID3D12CommandQueue = {0ec870a6-5d7e-4c22-8cfc-5baae07616ed}
	guidID3D12CommandQueue = [16]byte{
		0xa6, 0x70, 0xc8, 0x0e, 0x7e, 0x5d, 0x22, 0x4c,
		0x8c, 0xfc, 0x5b, 0xaa, 0xe0, 0x76, 0x16, 0xed,
	}
	// IID_ID3D12CommandAllocator = {6102dee4-af59-4b09-b999-b44d73f09b24}
	guidID3D12CommandAllocator = [16]byte{
		0xe4, 0xde, 0x02, 0x61, 0x59, 0xaf, 0x09, 0x4b,
		0xb9, 0x99, 0xb4, 0x4d, 0x73, 0xf0, 0x9b, 0x24,
	}
	// IID_ID3D12GraphicsCommandList = {5b160d0f-ac1b-4185-8ba8-b3ae42a5a455}
	guidID3D12GraphicsCommandList = [16]byte{
		0x0f, 0x0d, 0x16, 0x5b, 0x1b, 0xac, 0x85, 0x41,
		0x8b, 0xa8, 0xb3, 0xae, 0x42, 0xa5, 0xa4, 0x55,
	}
	// IID_ID3D12DescriptorHeap = {8efb471d-616c-4f49-90f7-127bb763fa51}
	guidID3D12DescriptorHeap = [16]byte{
		0x1d, 0x47, 0xfb, 0x8e, 0x6c, 0x61, 0x49, 0x4f,
		0x90, 0xf7, 0x12, 0x7b, 0xb7, 0x63, 0xfa, 0x51,
	}
	// IID_ID3D12Resource = {696442be-a72e-4059-bc79-5b5c98040fad}
	guidID3D12Resource = [16]byte{
		0xbe, 0x42, 0x64, 0x69, 0x2e, 0xa7, 0x59, 0x40,
		0xbc, 0x79, 0x5b, 0x5c, 0x98, 0x04, 0x0f, 0xad,
	}
	// IID_ID3D12Fence = {0a753dcf-c4d8-4b91-adf6-be5a60d95a76}
	guidID3D12Fence = [16]byte{
		0xcf, 0x3d, 0x75, 0x0a, 0xd8, 0xc4, 0x91, 0x4b,
		0xad, 0xf6, 0xbe, 0x5a, 0x60, 0xd9, 0x5a, 0x76,
	}
	// IID_IDXGIFactory4 = {1bc6ea02-ef36-464f-bf0c-21ca39e5168a}
	guidIDXGIFactory4 = [16]byte{
		0x02, 0xea, 0xc6, 0x1b, 0x36, 0xef, 0x4f, 0x46,
		0xbf, 0x0c, 0x21, 0xca, 0x39, 0xe5, 0x16, 0x8a,
	}
	// IID_IDXGISwapChain3 = {94d99bdb-f1f8-4ab0-b236-7da0170edab1}
	guidIDXGISwapChain3 = [16]byte{
		0xdb, 0x9b, 0xd9, 0x94, 0xf8, 0xf1, 0xb0, 0x4a,
		0xb2, 0x36, 0x7d, 0xa0, 0x17, 0x0e, 0xda, 0xb1,
	}
)

// HostD3D12 encapsule un vrai ID3D12Device Windows, une CommandQueue directe,
// une SwapChain DXGI3 double-buffering, un heap RTV, une command list de présentation
// et un fence de synchronisation CPU/GPU.
type HostD3D12 struct {
	mu           sync.Mutex
	Device       uintptr
	Queue        uintptr
	Factory      uintptr
	SwapChain    uintptr
	SwapChain3   uintptr
	HWND         uintptr
	Width        uint32
	Height       uint32
	RTVHeap      uintptr
	RTVStart     uintptr
	RTVStride    uintptr
	BackBuffers  [2]uintptr
	CmdAlloc     uintptr
	CmdList      uintptr
	Fence        uintptr
	FenceEvent   uintptr
	FenceValue   uint64
	UploadBuf     uintptr
	UploadPtr     uintptr
	UploadPitch   uint32
	UploadVB      uintptr
	UploadVBPtr   uintptr
	UploadVBGPU   uint64
	UploadIB      uintptr
	UploadIBPtr   uintptr
	UploadIBGPU   uint64
	UploadCB      uintptr
	UploadCBPtr   uintptr
	UploadCBGPU   uint64
	UploadTex      uintptr
	UploadTexPtr   uintptr
	texUploadOff   uint64
	ReplayRootSig  uintptr
	ReplayPSO      uintptr
	YUVPSO         uintptr
	ComputeRootSig uintptr
	ComputeUAVBuf  uintptr
	ComputeUAVGPU  uint64
	SRVHeap        uintptr
	SRVCPUStart   uintptr
	SRVGPUStart   uintptr
	SRVStride       uintptr
	NextSRVSlot     uint32
	NextRTVSlot     uint32
	TexCache        map[uint64]*hostTexEntry
	RTCache         map[uint64]*hostRTEntry
	PresentVAs      map[uint64]bool
	ReadbackBuf     uintptr
	ReadbackPtr     uintptr
	ReadbackPitch   uint32
	introFrameCount uint32
	introReached    bool
	introLogged     bool
	yuvLogged       bool
	splashBuf       []byte
	splashFrame     uint64
	splashPct       float32
	Ready           bool
}

var (
	hostMu       sync.Mutex
	hostInstance *HostD3D12
)

type d3d12CommandQueueDesc struct {
	Type     uint32
	Priority int32
	Flags    uint32
	NodeMask uint32
}

type d3d12DescriptorHeapDesc struct {
	Type           uint32
	NumDescriptors uint32
	Flags          uint32
	NodeMask       uint32
}

type d3d12HeapProperties struct {
	Type                 uint32
	CPUPageProperty      uint32
	MemoryPoolPreference uint32
	CreationNodeMask     uint32
	VisibleNodeMask      uint32
}

type d3d12ResourceDesc struct {
	Dimension        uint32
	_pad0            uint32
	Alignment        uint64
	Width            uint64
	Height           uint32
	DepthOrArraySize uint16
	MipLevels        uint16
	Format           uint32
	SampleCount      uint32
	SampleQuality    uint32
	Layout           uint32
	Flags            uint32
}

type d3d12ResourceBarrier struct {
	Type        uint32
	Flags       uint32
	Resource    uintptr
	Subresource uint32
	StateBefore uint32
	StateAfter  uint32
	_pad        uint32
}

type d3d12TextureCopyLocation struct {
	Resource  uintptr
	Type      uint32
	_pad0     uint32
	Offset    uint64 // SubresourceIndex (Type=0) ou Footprint.Offset (Type=1)
	Format    uint32
	Width     uint32
	Height    uint32
	Depth     uint32
	RowPitch  uint32
	_pad1     uint32
}

type dxgiSwapChainDesc1 struct {
	Width       uint32
	Height      uint32
	Format      uint32 // DXGI_FORMAT_R8G8B8A8_UNORM = 28
	Stereo      int32
	SampleCount uint32
	SampleQual  uint32
	BufferUsage uint32 // DXGI_USAGE_RENDER_TARGET_OUTPUT = 0x20
	BufferCount uint32
	Scaling     uint32
	SwapEffect  uint32 // DXGI_SWAP_EFFECT_FLIP_DISCARD = 4
	AlphaMode   uint32
	Flags       uint32
}

func comCall(obj uintptr, slot uintptr, args ...uintptr) uintptr {
	vtable := *(*uintptr)(*(*unsafe.Pointer)(unsafe.Pointer(&obj)))
	fn := *(*uintptr)(*(*unsafe.Pointer)(unsafe.Pointer(&vtable)))
	if slot > 0 {
		fnAddr := vtable + slot*8
		fn = *(*uintptr)(*(*unsafe.Pointer)(unsafe.Pointer(&fnAddr)))
	}
	all := make([]uintptr, 0, 1+len(args))
	all = append(all, obj)
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

// InitHostD3D12 initialise le vrai device D3D12 hôte Windows et attache une SwapChain DXGI3 à hwnd.
func InitHostD3D12(hwnd uintptr, width, height uint32) (*HostD3D12, error) {
	hostMu.Lock()
	defer hostMu.Unlock()
	if hostInstance != nil {
		return hostInstance, nil
	}
	if err := procD3D12CreateDevice.Find(); err != nil {
		return nil, fmt.Errorf("d3d12.dll indisponible : %w", err)
	}
	if err := procCreateDXGIFactory2.Find(); err != nil {
		return nil, fmt.Errorf("dxgi.dll indisponible : %w", err)
	}

	if os.Getenv("ASTRAL_D3D12_DEBUG") == "1" && procD3D12GetDebugIface.Find() == nil {
		var dbg uintptr
		if hrDbg, _, _ := procD3D12GetDebugIface.Call(uintptr(unsafe.Pointer(&guidID3D12Debug[0])), uintptr(unsafe.Pointer(&dbg))); int32(hrDbg) >= 0 && dbg != 0 {
			_ = comCall(dbg, 3) // EnableDebugLayer
			_ = comCall(dbg, 2) // Release
			slog.Info("Host D3D12 Debug Layer activé (ASTRAL_D3D12_DEBUG=1)")
		}
	}

	var device uintptr
	hr, _, _ := procD3D12CreateDevice.Call(
		0,      // adaptateur par défaut
		0xC000, // D3D_FEATURE_LEVEL_12_0
		uintptr(unsafe.Pointer(&guidID3D12Device[0])),
		uintptr(unsafe.Pointer(&device)),
	)
	if int32(hr) < 0 || device == 0 {
		return nil, fmt.Errorf("D3D12CreateDevice échoué (HRESULT 0x%08X)", uint32(hr))
	}

	qDesc := d3d12CommandQueueDesc{Type: 0 /* DIRECT */}
	var queue uintptr
	// ID3D12Device::CreateCommandQueue = slot 8
	hr = comCall(device, 8, uintptr(unsafe.Pointer(&qDesc)), uintptr(unsafe.Pointer(&guidID3D12CommandQueue[0])), uintptr(unsafe.Pointer(&queue)))
	if int32(hr) < 0 || queue == 0 {
		return nil, fmt.Errorf("CreateCommandQueue hôte échoué (HRESULT 0x%08X)", uint32(hr))
	}

	var factory uintptr
	hr, _, _ = procCreateDXGIFactory2.Call(
		0,
		uintptr(unsafe.Pointer(&guidIDXGIFactory4[0])),
		uintptr(unsafe.Pointer(&factory)),
	)
	if int32(hr) < 0 || factory == 0 {
		return nil, fmt.Errorf("CreateDXGIFactory2 échoué (HRESULT 0x%08X)", uint32(hr))
	}

	h := &HostD3D12{
		Device:  device,
		Queue:   queue,
		Factory: factory,
		HWND:    hwnd,
		Width:   width,
		Height:  height,
	}

	if hwnd != 0 {
		scDesc := dxgiSwapChainDesc1{
			Width:       width,
			Height:      height,
			Format:      28, // DXGI_FORMAT_R8G8B8A8_UNORM
			SampleCount: 1,
			BufferUsage: 0x20, // DXGI_USAGE_RENDER_TARGET_OUTPUT
			BufferCount: 2,
			SwapEffect:  4, // DXGI_SWAP_EFFECT_FLIP_DISCARD
		}
		// IDXGIFactory2::CreateSwapChainForHwnd = slot 15
		hr = comCall(factory, 15, queue, hwnd, uintptr(unsafe.Pointer(&scDesc)), 0, 0, uintptr(unsafe.Pointer(&h.SwapChain)))
		if int32(hr) < 0 {
			slog.Warn("CreateSwapChainForHwnd hôte échoué (poursuite sans swapchain)", "hr", fmt.Sprintf("0x%08X", uint32(hr)))
		} else {
			// QueryInterface -> IDXGISwapChain3 (slot 0)
			_ = comCall(h.SwapChain, 0, uintptr(unsafe.Pointer(&guidIDXGISwapChain3[0])), uintptr(unsafe.Pointer(&h.SwapChain3)))
			h.initPresentPipeline()
		}
	}

	hostInstance = h
	slog.Info("Host D3D12 + SwapChain DXGI initialisés",
		"device", fmt.Sprintf("0x%X", device),
		"swapchain", fmt.Sprintf("0x%X", h.SwapChain),
		"presentReady", h.Ready,
	)
	return hostInstance, nil
}

func (h *HostD3D12) initPresentPipeline() {
	if h.Device == 0 || h.SwapChain3 == 0 {
		return
	}
	// 1. Heap RTV (64 descripteurs : 0..1 = double-buffering SwapChain, 2..63 = RenderTargets offscreen)
	rtvDesc := d3d12DescriptorHeapDesc{Type: 2 /* RTV */, NumDescriptors: 64}
	if int32(comCall(h.Device, 14, uintptr(unsafe.Pointer(&rtvDesc)), uintptr(unsafe.Pointer(&guidID3D12DescriptorHeap[0])), uintptr(unsafe.Pointer(&h.RTVHeap)))) < 0 || h.RTVHeap == 0 {
		return
	}
	h.RTVStride = comCall(h.Device, 15, 2 /* RTV */)
	// ID3D12DescriptorHeap::GetCPUDescriptorHandleForHeapStart (MSVC sret dans rdx)
	var rtvStart uintptr
	_ = comCall(h.RTVHeap, 9, uintptr(unsafe.Pointer(&rtvStart)))
	h.RTVStart = rtvStart

	for i := uintptr(0); i < 2; i++ {
		var bb uintptr
		if int32(comCall(h.SwapChain, 9, i, uintptr(unsafe.Pointer(&guidID3D12Resource[0])), uintptr(unsafe.Pointer(&bb)))) < 0 || bb == 0 {
			return
		}
		h.BackBuffers[i] = bb
		// ID3D12Device::CreateRenderTargetView (slot 20)
		_ = comCall(h.Device, 20, bb, 0, h.RTVStart+i*h.RTVStride)
	}

	// 2. CommandAllocator + CommandList (fermée au repos)
	if int32(comCall(h.Device, 9, 0 /* DIRECT */, uintptr(unsafe.Pointer(&guidID3D12CommandAllocator[0])), uintptr(unsafe.Pointer(&h.CmdAlloc)))) < 0 || h.CmdAlloc == 0 {
		return
	}
	if int32(comCall(h.Device, 12, 0, 0 /* DIRECT */, h.CmdAlloc, 0, uintptr(unsafe.Pointer(&guidID3D12GraphicsCommandList[0])), uintptr(unsafe.Pointer(&h.CmdList)))) < 0 || h.CmdList == 0 {
		return
	}
	_ = comCall(h.CmdList, 9) // Close

	// 3. Fence + Event CPU/GPU
	if int32(comCall(h.Device, 36, 0, 0, uintptr(unsafe.Pointer(&guidID3D12Fence[0])), uintptr(unsafe.Pointer(&h.Fence)))) < 0 || h.Fence == 0 {
		return
	}
	h.FenceEvent, _, _ = procCreateEventW.Call(0, 0, 0, 0)

	// 4. Buffer d'upload persistant pour copier un framebuffer RGBA8 vers la SwapChain
	pitch := ((h.Width * 4) + 255) &^ 255
	uploadSize := uint64(pitch) * uint64(h.Height)
	heapProps := d3d12HeapProperties{Type: 2 /* UPLOAD */, CreationNodeMask: 1, VisibleNodeMask: 1}
	bufDesc := d3d12ResourceDesc{
		Dimension:        1, // BUFFER
		Width:            uploadSize,
		Height:           1,
		DepthOrArraySize: 1,
		MipLevels:        1,
		SampleCount:      1,
		Layout:           1, // ROW_MAJOR
	}
	var upBuf uintptr
	if int32(comCall(h.Device, 27,
		uintptr(unsafe.Pointer(&heapProps)), 0,
		uintptr(unsafe.Pointer(&bufDesc)),
		0x00000A83, // D3D12_RESOURCE_STATE_GENERIC_READ
		0,
		uintptr(unsafe.Pointer(&guidID3D12Resource[0])),
		uintptr(unsafe.Pointer(&upBuf)),
	)) >= 0 && upBuf != 0 {
		var mapped uintptr
		// ID3D12Resource::Map (slot 8)
		if int32(comCall(upBuf, 8, 0, 0, uintptr(unsafe.Pointer(&mapped)))) >= 0 && mapped != 0 {
			h.UploadBuf = upBuf
			h.UploadPtr = mapped
			h.UploadPitch = pitch
		}
	}

	// 5. Buffer READBACK optionnel pour capture d'écran (ASTRAL_SCREENSHOT=1)
	if os.Getenv("ASTRAL_SCREENSHOT") == "1" {
		rbHeap := d3d12HeapProperties{Type: 3 /* READBACK */, CreationNodeMask: 1, VisibleNodeMask: 1}
		var rbBuf uintptr
		if int32(comCall(h.Device, 27,
			uintptr(unsafe.Pointer(&rbHeap)), 0,
			uintptr(unsafe.Pointer(&bufDesc)),
			0x400, // D3D12_RESOURCE_STATE_COPY_DEST
			0,
			uintptr(unsafe.Pointer(&guidID3D12Resource[0])),
			uintptr(unsafe.Pointer(&rbBuf)),
		)) >= 0 && rbBuf != 0 {
			var rbMapped uintptr
			if int32(comCall(rbBuf, 8, 0, 0, uintptr(unsafe.Pointer(&rbMapped)))) >= 0 && rbMapped != 0 {
				h.ReadbackBuf = rbBuf
				h.ReadbackPtr = rbMapped
				h.ReadbackPitch = pitch
			}
		}
	}

	h.initReplayPipeline()
	h.Ready = true
	// Affiche immédiatement le splash GTA VI Astral et l'anime en douceur jusqu'à l'arrivée du jeu
	if h.UploadBuf != 0 && h.UploadPtr != 0 {
		go func() {
			h.Present(0)
			ticker := time.NewTicker(40 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				if IsIntroReached() {
					return
				}
				h.Present(0)
			}
		}()
	}
}

// DeviceRemovedReason interroge ID3D12Device::GetDeviceRemovedReason (slot 37) pour le diagnostic GPU.
func (h *HostD3D12) DeviceRemovedReason() uint32 {
	if h == nil || h.Device == 0 {
		return 0
	}
	return uint32(comCall(h.Device, 37))
}

func (h *HostD3D12) waitGPU() {
	if h.Queue == 0 || h.Fence == 0 {
		return
	}
	h.FenceValue++
	val := h.FenceValue
	hr := comCall(h.Queue, 14, h.Fence, uintptr(val)) // Signal
	if int32(hr) < 0 {
		slog.Error("Host D3D12 Queue::Signal échoué", "hr", fmt.Sprintf("0x%08X", uint32(hr)), "removedReason", fmt.Sprintf("0x%08X", h.DeviceRemovedReason()))
		return
	}
	if uint64(comCall(h.Fence, 8)) < val { // GetCompletedValue
		_ = comCall(h.Fence, 9, uintptr(val), h.FenceEvent) // SetEventOnCompletion
		_, _, _ = procWaitForSingleObj.Call(h.FenceEvent, 2000)
	}
}

// PresentClear présente la trame courante en effaçant le backbuffer avec clearColor (RGBA),
// en rejouant les Compute Dispatches + DrawInstanced hôtes (avec support des RenderTargets offscreen -> SRV),
// puis en appelant Present.
func (h *HostD3D12) PresentClear(syncInterval uint32, clearColor [4]float32, presentVA uint64, srcRGBA []byte, srcW, srcH, srcPitch uint32, dispatches []HostDispatchPass, draws ...HostDrawPass) {
	if h == nil || h.SwapChain == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.Ready {
		// Enregistre systématiquement chaque backbuffer swapchain dès la trame 1 (y compris pendant le splash)
		if presentVA != 0 {
			if h.PresentVAs == nil {
				h.PresentVAs = make(map[uint64]bool)
			}
			h.PresentVAs[presentVA] = true
		}
		// Après l'apparition de l'UI (introReached), ne jamais écraser l'écran par un Present vide tout noir
		if h.introReached && len(draws) == 0 && len(srcRGBA) == 0 {
			return
		}
		idx := comCall(h.SwapChain3, 36) & 1 // GetCurrentBackBufferIndex
		bb := h.BackBuffers[idx]
		rtv := h.RTVStart + idx*h.RTVStride

		_ = comCall(h.CmdAlloc, 8)                // Allocator::Reset
		_ = comCall(h.CmdList, 10, h.CmdAlloc, 0) // CommandList::Reset

		// Pendant le chargement initial (avant l'arrivée de l'interface Scaleform à 8+ vrais draws/trame),
		// affiche le logo GTA VI Astral + barre de progression néon animée sur le backbuffer.
		realDraws := 0
		for _, d := range draws {
			if !d.IsClear && d.VertexCount > 0 {
				realDraws++
			}
		}
		if realDraws >= 8 {
			h.introReached = true
			markIntroReached()
			ReportBootProgress(1.0)
		} else if len(dispatches) > 0 {
			ReportBootProgress(0.92)
		}
		if !h.introReached && len(srcRGBA) == 0 && h.UploadBuf != 0 && h.UploadPtr != 0 {
			if splash := h.renderSplashRGBA(); len(splash) > 0 {
				srcRGBA = splash
				srcW = h.Width
				srcH = h.Height
				srcPitch = h.Width * 4
			}
		}

		if len(srcRGBA) > 0 && h.UploadBuf != 0 && h.UploadPtr != 0 && srcW > 0 && srcH > 0 {
			copyW := srcW
			if copyW > h.Width {
				copyW = h.Width
			}
			copyH := srcH
			if copyH > h.Height {
				copyH = h.Height
			}
			rowBytes := copyW * 4
			if srcPitch < rowBytes {
				srcPitch = rowBytes
			}
			dstSlice := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&h.UploadPtr))), uintptr(h.UploadPitch)*uintptr(h.Height))
			for y := uint32(0); y < copyH; y++ {
				sOff := uintptr(y) * uintptr(srcPitch)
				dOff := uintptr(y) * uintptr(h.UploadPitch)
				if sOff+uintptr(rowBytes) <= uintptr(len(srcRGBA)) && dOff+uintptr(rowBytes) <= uintptr(len(dstSlice)) {
					copy(dstSlice[dOff:dOff+uintptr(rowBytes)], srcRGBA[sOff:sOff+uintptr(rowBytes)])
				}
			}

			b1 := d3d12ResourceBarrier{Resource: bb, Subresource: 0xFFFFFFFF, StateBefore: 0 /* PRESENT */, StateAfter: 0x400 /* COPY_DEST */}
			_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&b1)))

			dstLoc := d3d12TextureCopyLocation{Resource: bb, Type: 0 /* SUBRESOURCE_INDEX */}
			srcLoc := d3d12TextureCopyLocation{
				Resource: h.UploadBuf,
				Type:     1, // PLACED_FOOTPRINT
				Format:   28,
				Width:    h.Width,
				Height:   h.Height,
				Depth:    1,
				RowPitch: h.UploadPitch,
			}
			_ = comCall(h.CmdList, 16, uintptr(unsafe.Pointer(&dstLoc)), 0, 0, 0, uintptr(unsafe.Pointer(&srcLoc)), 0)

			b2 := d3d12ResourceBarrier{Resource: bb, Subresource: 0xFFFFFFFF, StateBefore: 0x400 /* COPY_DEST */, StateAfter: 0 /* PRESENT */}
			_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&b2)))
		} else {
			b1 := d3d12ResourceBarrier{Resource: bb, Subresource: 0xFFFFFFFF, StateBefore: 0 /* PRESENT */, StateAfter: 0x4 /* RENDER_TARGET */}
			_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&b1)))

			// ID3D12GraphicsCommandList::ClearRenderTargetView (slot 48)
			_ = comCall(h.CmdList, 48, rtv, uintptr(unsafe.Pointer(&clearColor[0])), 0, 0)

			// Rejeu matériel des Compute Shaders (Dispatch) sur le GPU Windows hôte
			if len(dispatches) > 0 && h.ComputeRootSig != 0 && h.ComputeUAVGPU != 0 {
				_ = comCall(h.CmdList, 29, h.ComputeRootSig) // SetComputeRootSignature
				_ = comCall(h.CmdList, 41, 1, uintptr(h.ComputeUAVGPU)) // SetComputeRootUnorderedAccessView(param=1, u0)
				for idxCS, cs := range dispatches {
					if cs.HostPSO == 0 || cs.X == 0 || cs.Y == 0 || cs.Z == 0 {
						continue
					}
					if h.UploadCBPtr != 0 && h.UploadCBGPU != 0 {
						cbSlot := uint64((idxCS+480)%512) * 16384
						dstCB := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&h.UploadCBPtr))), 8<<20)[cbSlot : cbSlot+16384]
						clear(dstCB)
						if len(cs.CBData) > 0 {
							copy(dstCB, cs.CBData)
						}
						_ = comCall(h.CmdList, 37, 0, uintptr(h.UploadCBGPU+cbSlot)) // SetComputeRootConstantBufferView(param=0, b0)
					}
					_ = comCall(h.CmdList, 25, cs.HostPSO) // SetPipelineState
					gx, gy, gz := cs.X, cs.Y, cs.Z
					if gx > 1024 {
						gx = 1024
					}
					if gy > 1024 {
						gy = 1024
					}
					if gz > 64 {
						gz = 64
					}
					_ = comCall(h.CmdList, 14, uintptr(gx), uintptr(gy), uintptr(gz)) // Dispatch
				}
			}

			// Rejeu matériel des appels DrawInstanced sur le GPU Windows hôte
			h.texUploadOff = 0
			skipHostPresent := false
			if len(draws) > 0 && h.ReplayRootSig != 0 && h.ReplayPSO != 0 {
				vp := d3d12Viewport{Width: float32(h.Width), Height: float32(h.Height), MinDepth: 0, MaxDepth: 1}
				sc := d3d12Rect{Right: int32(h.Width), Bottom: int32(h.Height)}
				_ = comCall(h.CmdList, 46, 1, uintptr(unsafe.Pointer(&rtv)), 0, 0) // OMSetRenderTargets
				_ = comCall(h.CmdList, 21, 1, uintptr(unsafe.Pointer(&vp)))        // RSSetViewports
				_ = comCall(h.CmdList, 22, 1, uintptr(unsafe.Pointer(&sc)))        // RSSetScissorRects
				_ = comCall(h.CmdList, 20, 4)                                      // IASetPrimitiveTopology(TRIANGLELIST)
				if h.SRVHeap != 0 {
					heaps := [1]uintptr{h.SRVHeap}
					_ = comCall(h.CmdList, 28, 1, uintptr(unsafe.Pointer(&heaps[0]))) // SetDescriptorHeaps
				}
				_ = comCall(h.CmdList, 30, h.ReplayRootSig) // SetGraphicsRootSignature
				if h.SRVGPUStart != 0 {
					_ = comCall(h.CmdList, 32, 1, h.SRVGPUStart) // t0
					_ = comCall(h.CmdList, 32, 3, h.SRVGPUStart) // t1
					_ = comCall(h.CmdList, 32, 4, h.SRVGPUStart) // t2
					_ = comCall(h.CmdList, 32, 5, h.SRVGPUStart) // t3
				}
				total := uint32(len(draws))
				if presentVA != 0 {
					if h.PresentVAs == nil {
						h.PresentVAs = make(map[uint64]bool)
					}
					h.PresentVAs[presentVA] = true
				}
				isBackbuffer := func(va uint64) bool {
					return va == 0 || va == presentVA || h.PresentVAs[va]
				}
				hasRealPSO := false
				hasPresentTarget := false
				for _, d := range draws {
					if d.IsClear {
						continue
					}
					if d.HostPSO != 0 {
						hasRealPSO = true
					}
					if d.TargetVA != 0 && isBackbuffer(d.TargetVA) {
						hasPresentTarget = true
					}
				}
				// Si l'UI est déjà active et qu'aucun draw de cette trame ne cible le backbuffer,
				// ne pas effacer ni présenter un backbuffer noir : mettre à jour uniquement les RT offscreen.
				skipHostPresent = h.introReached && len(h.PresentVAs) > 0 && !hasPresentTarget
				if len(draws) > 0 && (draws[0].FrameNumber%120 == 0 || (h.introReached && !h.introLogged)) {
					slog.Info("HostD3D12 PresentClear",
						"frame", draws[0].FrameNumber,
						"draws", len(draws),
						"presentVA", fmt.Sprintf("0x%X", presentVA),
						"hasRealPSO", hasRealPSO,
						"hasPresentTarget", hasPresentTarget,
						"skipHostPresent", skipHostPresent,
					)
					if h.introReached && !h.introLogged {
						h.introLogged = true
						for i, d := range draws {
							if i >= 24 {
								break
							}
							slog.Info("HostD3D12 IntroDraw",
								"i", i,
								"pso", fmt.Sprintf("0x%X", d.HostPSO),
								"tgt", fmt.Sprintf("0x%X", d.TargetVA),
								"srv", fmt.Sprintf("0x%X", d.SRVGuestVA),
								"cnt", d.VertexCount,
								"vb", len(d.VBData),
								"cb", len(d.CBData),
								"pscb", len(d.PSCBData),
								"hasTex", d.SRVTex != nil,
							)
						}
					}
				}
				// N'active les RenderTargets offscreen que si des backbuffers swapchain sont connus
				useOffscreenRT := len(h.PresentVAs) > 0 && (hasPresentTarget || skipHostPresent)
				curRTV := rtv
				curTargetVA := uint64(0)
				vbOff := uint64(0)
				ibOff := uint64(0)
				for idxPass, d := range draws {
					if d.IsClear {
						if useOffscreenRT && d.TargetVA != 0 && !isBackbuffer(d.TargetVA) {
							if rt := h.ensureHostRT(d.TargetVA); rt != nil {
								if rt.state != 0x4 {
									if h.SRVGPUStart != 0 {
										_ = comCall(h.CmdList, 32, 1, h.SRVGPUStart)
									}
									br := d3d12ResourceBarrier{Resource: rt.res, Subresource: 0xFFFFFFFF, StateBefore: rt.state, StateAfter: 0x4 /* RENDER_TARGET */}
									_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&br)))
									rt.state = 0x4
								}
								clr := d.ClearColor
								_ = comCall(h.CmdList, 48, rt.rtvCPU, uintptr(unsafe.Pointer(&clr[0])), 0, 0)
								rt.initClear = true
							}
						}
						continue
					}
					if d.VertexCount == 0 || d.InstanceCount == 0 {
						continue
					}
					pso := d.HostPSO
					if pso == 0 {
						if hasRealPSO {
							continue
						}
						pso = h.ReplayPSO
					}
					// 1. Sélection de la cible RTV (backbuffer swapchain ou RenderTarget offscreen)
					wantRTV := rtv
					wantTargetVA := uint64(0)
					if useOffscreenRT && !isBackbuffer(d.TargetVA) {
						if rt := h.ensureHostRT(d.TargetVA); rt != nil {
							if rt.state != 0x4 {
								if h.SRVGPUStart != 0 {
									_ = comCall(h.CmdList, 32, 1, h.SRVGPUStart) // Détache le SRV avant transition -> RENDER_TARGET
								}
								br := d3d12ResourceBarrier{Resource: rt.res, Subresource: 0xFFFFFFFF, StateBefore: rt.state, StateAfter: 0x4 /* RENDER_TARGET */}
								_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&br)))
								rt.state = 0x4
							}
							// Ne jamais effacer un RT offscreen en début de batch PresentX (cela effacerait
							// l'image de la trame N-1 avant son composite !). Uniquement à la toute 1re création
							// ou sur un vrai ClearRenderTargetView enregistré dans l'ordre du jeu.
							if !rt.initClear {
								zeroClr := [4]float32{0, 0, 0, 0}
								_ = comCall(h.CmdList, 48, rt.rtvCPU, uintptr(unsafe.Pointer(&zeroClr[0])), 0, 0)
								rt.initClear = true
							}
							wantRTV = rt.rtvCPU
							wantTargetVA = d.TargetVA
						}
					}
					if wantRTV != curRTV {
						_ = comCall(h.CmdList, 46, 1, uintptr(unsafe.Pointer(&wantRTV)), 0, 0) // OMSetRenderTargets
						curRTV = wantRTV
						curTargetVA = wantTargetVA
					}
					// 2. Liaison du SRV t0 : priorité au RenderTarget offscreen hôte si SRVGuestVA correspond !
					if d.SRVGuestVA != 0 && d.SRVGuestVA != curTargetVA {
						if srcRT, ok := h.RTCache[d.SRVGuestVA]; ok && srcRT != nil {
							// Si ce draw est un composite plein écran (3 sommets) vers le backbuffer hôte OU un autre RT offscreen
							// (ex: vidéo 4K YUV 0xB58000000 -> 0x105B7A0000 -> backbuffer), copier directement srcRT -> dstRes
							// par CopyResource (évite la matrice de projection manquante du 2e CBV dans les VS 45/45 ins).
							if !d.Indexed && d.VertexCount == 3 {
								var dstRes uintptr
								var dstRT *hostRTEntry
								if curTargetVA == 0 {
									dstRes = bb
								} else if drt, dok := h.RTCache[curTargetVA]; dok && drt != nil {
									dstRes = drt.res
									dstRT = drt
								}
								if dstRes != 0 && dstRes != srcRT.res {
									if srcRT.state != 0x800 {
										brSrc := d3d12ResourceBarrier{Resource: srcRT.res, Subresource: 0xFFFFFFFF, StateBefore: srcRT.state, StateAfter: 0x800 /* COPY_SOURCE */}
										_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&brSrc)))
										srcRT.state = 0x800
									}
									brDst1 := d3d12ResourceBarrier{Resource: dstRes, Subresource: 0xFFFFFFFF, StateBefore: 0x4 /* RENDER_TARGET */, StateAfter: 0x400 /* COPY_DEST */}
									_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&brDst1)))
									// ID3D12GraphicsCommandList::CopyResource (slot 17)
									_ = comCall(h.CmdList, 17, dstRes, srcRT.res)
									brDst2 := d3d12ResourceBarrier{Resource: dstRes, Subresource: 0xFFFFFFFF, StateBefore: 0x400 /* COPY_DEST */, StateAfter: 0x4 /* RENDER_TARGET */}
									_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&brDst2)))
									if dstRT != nil {
										dstRT.state = 0x4
									}
									continue
								}
							}
							if srcRT.state != 0xC0 {
								br := d3d12ResourceBarrier{Resource: srcRT.res, Subresource: 0xFFFFFFFF, StateBefore: srcRT.state, StateAfter: 0xC0 /* NON_PIXEL | PIXEL_SHADER_RESOURCE */}
								_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&br)))
								srcRT.state = 0xC0
							}
							_ = comCall(h.CmdList, 32, 1, srcRT.srvGPU)
							goto srvBound
						}
					}
					if d.SRVTex != nil {
						srvGPU := h.ensureHostTexture(d.SRVTex)
						if srvGPU != 0 {
							_ = comCall(h.CmdList, 32, 1, srvGPU) // SetGraphicsRootDescriptorTable(param=1, t0)
						}
					} else if h.SRVGPUStart != 0 {
						_ = comCall(h.CmdList, 32, 1, h.SRVGPUStart)
					}
				srvBound:
					// Liaison des SRV supplémentaires t1..t3 (params 3..5)
					for ei := 0; ei < 3; ei++ {
						paramIdx := uintptr(3 + ei)
						if d.ExtraSRVs[ei] != nil {
							if eg := h.ensureHostTexture(d.ExtraSRVs[ei]); eg != 0 {
								_ = comCall(h.CmdList, 32, paramIdx, eg)
								continue
							}
						}
						if h.SRVGPUStart != 0 {
							_ = comCall(h.CmdList, 32, paramIdx, h.SRVGPUStart)
						}
					}
					// Passe vidéo Bink2 4K YUV420p (Y=t0 3840x2160, U=t1 1920x1080, V=t2 1920x1080 en R8_UNORM) :
					// utiliser notre PSO BT.709 matériel dédié pour garantir un décodage couleur complet sans
					// dépendre des 3 opcodes manquants du shader invité 73/76.
					if h.YUVPSO != 0 && d.SRVTex != nil && d.ExtraSRVs[0] != nil && d.ExtraSRVs[1] != nil &&
						d.SRVTex.Format == 61 && d.SRVTex.Width >= 1920 &&
						d.ExtraSRVs[0].Width == d.SRVTex.Width/2 && d.ExtraSRVs[1].Width == d.SRVTex.Width/2 {
						if !h.yuvLogged {
							h.yuvLogged = true
							slog.Info("HostD3D12 Bink2 4K YUV vidéo actif (PSO BT.709)",
								"y", fmt.Sprintf("%dx%d", d.SRVTex.Width, d.SRVTex.Height),
								"u", fmt.Sprintf("%dx%d", d.ExtraSRVs[0].Width, d.ExtraSRVs[0].Height),
								"v", fmt.Sprintf("%dx%d", d.ExtraSRVs[1].Width, d.ExtraSRVs[1].Height),
								"tgt", fmt.Sprintf("0x%X", curTargetVA))
						}
						_ = comCall(h.CmdList, 20, 4)        // TRIANGLELIST
						_ = comCall(h.CmdList, 25, h.YUVPSO) // SetPipelineState(YUVPSO)
						_ = comCall(h.CmdList, 12, 3, 1, 0, 0)
						continue
					}
					topo := d.Topology
					if topo == 0 || topo == 17 || topo >= 32 {
						// Topologie par défaut ou Xbox RECTLIST -> TRIANGLELIST (4) sur PC
						topo = 4
					}
					_ = comCall(h.CmdList, 20, uintptr(topo)) // IASetPrimitiveTopology
					_ = comCall(h.CmdList, 25, pso)           // SetPipelineState
					// Upload Constant Buffers VS (b0, param 0) et PS (b1, param 2) : 16 KiB alignés chacun
					if h.UploadCBPtr != 0 && h.UploadCBGPU != 0 {
						cbAll := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&h.UploadCBPtr))), 16<<20)
						vsSlot := uint64((idxPass*2)%1024) * 16384
						dstVS := cbAll[vsSlot : vsSlot+16384]
						clear(dstVS)
						if len(d.CBData) > 0 {
							copy(dstVS, d.CBData)
						} else {
							*(*uint32)(unsafe.Pointer(&dstVS[0])) = d.FrameNumber
							*(*uint32)(unsafe.Pointer(&dstVS[4])) = d.PassIndex
							*(*uint32)(unsafe.Pointer(&dstVS[8])) = total
						}
						_ = comCall(h.CmdList, 38, 0, uintptr(h.UploadCBGPU+vsSlot)) // b0 (VS)

						psSlot := uint64((idxPass*2+1)%1024) * 16384
						dstPS := cbAll[psSlot : psSlot+16384]
						clear(dstPS)
						if len(d.PSCBData) > 0 {
							copy(dstPS, d.PSCBData)
						} else if len(d.CBData) > 0 {
							copy(dstPS, d.CBData)
						}
						_ = comCall(h.CmdList, 38, 2, uintptr(h.UploadCBGPU+psSlot)) // b1 (PS)
					}
					// Expansion Xbox RECTLIST (topo=17, 3 sommets par rectangle -> 2 triangles = 6 sommets, tout stride >= 12)
					// Détecte automatiquement le sommet d'angle droit (V0, V1 ou V2) via produit scalaire pour supporter tous les ordres Xbox.
					drawVB := d.VBData
					drawCnt := d.VertexCount
					if !d.Indexed && (d.Topology == 17 || d.Topology >= 32) && d.VertexCount >= 3 && d.VertexCount%3 == 0 && d.VBStride >= 12 && d.VBStride%4 == 0 && uint32(len(d.VBData)) >= d.VertexCount*d.VBStride {
						st := d.VBStride
						numRects := d.VertexCount / 3
						expanded := make([]byte, numRects*6*st)
						v3 := make([]byte, st)
						absF := func(x float32) float32 {
							if x < 0 {
								return -x
							}
							return x
						}
						for r := uint32(0); r < numRects; r++ {
							v0 := d.VBData[(r*3+0)*st : (r*3+1)*st]
							v1 := d.VBData[(r*3+1)*st : (r*3+2)*st]
							v2 := d.VBData[(r*3+2)*st : (r*3+3)*st]
							f0 := unsafe.Slice((*float32)(unsafe.Pointer(&v0[0])), st/4)
							f1 := unsafe.Slice((*float32)(unsafe.Pointer(&v1[0])), st/4)
							f2 := unsafe.Slice((*float32)(unsafe.Pointer(&v2[0])), st/4)
							px, py := 0, 1
							if st == 12 {
								px, py = 1, 2
							}
							// Produit scalaire des 2 arêtes à chaque sommet : le plus proche de 0 est l'angle droit
							d0 := absF((f1[px]-f0[px])*(f2[px]-f0[px]) + (f1[py]-f0[py])*(f2[py]-f0[py]))
							d1 := absF((f0[px]-f1[px])*(f2[px]-f1[px]) + (f0[py]-f1[py])*(f2[py]-f1[py]))
							d2 := absF((f0[px]-f2[px])*(f1[px]-f2[px]) + (f0[py]-f2[py])*(f1[py]-f2[py]))
							// a, b = les 2 sommets de la diagonale, c = le sommet d'angle droit -> V3 = a + b - c
							va, vb, vc := v1, v2, v0
							fa, fb, fc := f1, f2, f0
							if d1 <= d0 && d1 <= d2 {
								va, vb, vc = v0, v2, v1
								fa, fb, fc = f0, f2, f1
							} else if d2 <= d0 && d2 <= d1 {
								va, vb, vc = v0, v1, v2
								fa, fb, fc = f0, f1, f2
							}
							copy(v3, vb)
							f3 := unsafe.Slice((*float32)(unsafe.Pointer(&v3[0])), st/4)
							if st == 12 {
								f3[1] = fa[1] + fb[1] - fc[1]
								f3[2] = fa[2] + fb[2] - fc[2]
							} else {
								f3[0] = fa[0] + fb[0] - fc[0]
								f3[1] = fa[1] + fb[1] - fc[1]
								f3[2] = fa[2] + fb[2] - fc[2]
								if st == 24 {
									f3[4] = fa[4] + fb[4] - fc[4]
									f3[5] = fa[5] + fb[5] - fc[5]
								} else if st == 20 {
									f3[3] = fa[3] + fb[3] - fc[3]
									f3[4] = fa[4] + fb[4] - fc[4]
								}
							}
							base := r * 6 * st
							copy(expanded[base+0*st:], vc)
							copy(expanded[base+1*st:], va)
							copy(expanded[base+2*st:], vb)
							copy(expanded[base+3*st:], va)
							copy(expanded[base+4*st:], v3)
							copy(expanded[base+5*st:], vb)
						}
						drawVB = expanded
						drawCnt = numRects * 6
					}
					// Upload Vertex Buffer si fourni par le titre
					if len(drawVB) > 0 && d.VBStride > 0 && h.UploadVBPtr != 0 && h.UploadVBGPU != 0 {
						nBytes := uint64(len(drawVB))
						if vbOff+nBytes <= (32 << 20) {
							dstVB := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&h.UploadVBPtr))), 32<<20)[vbOff : vbOff+nBytes]
							copy(dstVB, drawVB)
							vbv := d3d12VertexBufferView{
								BufferLocation: h.UploadVBGPU + vbOff,
								SizeInBytes:    uint32(nBytes),
								StrideInBytes:  d.VBStride,
							}
							// ID3D12GraphicsCommandList::IASetVertexBuffers (slot 44)
							_ = comCall(h.CmdList, 44, 0, 1, uintptr(unsafe.Pointer(&vbv)))
							vbOff = (vbOff + nBytes + 255) &^ 255
						}
					}
					if d.Indexed && len(d.IBData) > 0 && h.UploadIBPtr != 0 && h.UploadIBGPU != 0 {
						nBytes := uint64(len(d.IBData))
						if ibOff+nBytes <= (16 << 20) {
							dstIB := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&h.UploadIBPtr))), 16<<20)[ibOff : ibOff+nBytes]
							copy(dstIB, d.IBData)
							ibFmt := d.IBFormat
							if ibFmt != 57 && ibFmt != 42 {
								ibFmt = 57 // DXGI_FORMAT_R16_UINT
							}
							ibv := d3d12IndexBufferView{
								BufferLocation: h.UploadIBGPU + ibOff,
								SizeInBytes:    uint32(nBytes),
								Format:         ibFmt,
							}
							// ID3D12GraphicsCommandList::IASetIndexBuffer (slot 43)
							_ = comCall(h.CmdList, 43, uintptr(unsafe.Pointer(&ibv)))
							ibOff = (ibOff + nBytes + 255) &^ 255
							// ID3D12GraphicsCommandList::DrawIndexedInstanced (slot 13)
							_ = comCall(h.CmdList, 13, uintptr(d.VertexCount), uintptr(d.InstanceCount), uintptr(d.StartVertex), uintptr(d.BaseVertex), uintptr(d.StartInstance))
							continue
						}
					}
					_ = comCall(h.CmdList, 12, uintptr(drawCnt), uintptr(d.InstanceCount), uintptr(d.StartVertex), uintptr(d.StartInstance))
				}
			}

			wantSnap := false
			if h.ReadbackBuf != 0 && h.ReadbackPtr != 0 && h.introReached {
				h.introFrameCount++
				if h.introFrameCount == 1 || h.introFrameCount == 90 {
					wantSnap = true
					bSnap1 := d3d12ResourceBarrier{Resource: bb, Subresource: 0xFFFFFFFF, StateBefore: 0x4 /* RENDER_TARGET */, StateAfter: 0x800 /* COPY_SOURCE */}
					_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&bSnap1)))
					dstLoc := d3d12TextureCopyLocation{
						Resource: h.ReadbackBuf,
						Type:     1, // PLACED_FOOTPRINT
						Format:   28,
						Width:    h.Width,
						Height:   h.Height,
						Depth:    1,
						RowPitch: h.ReadbackPitch,
					}
					srcLoc := d3d12TextureCopyLocation{Resource: bb, Type: 0 /* SUBRESOURCE_INDEX */}
					_ = comCall(h.CmdList, 16, uintptr(unsafe.Pointer(&dstLoc)), 0, 0, 0, uintptr(unsafe.Pointer(&srcLoc)), 0)
					bSnap2 := d3d12ResourceBarrier{Resource: bb, Subresource: 0xFFFFFFFF, StateBefore: 0x800 /* COPY_SOURCE */, StateAfter: 0 /* PRESENT */}
					_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&bSnap2)))
				}
			}
			if !wantSnap {
				if skipHostPresent {
				// On met uniquement à jour les RenderTargets offscreen sans toucher au backbuffer affiché
				b2 := d3d12ResourceBarrier{Resource: bb, Subresource: 0xFFFFFFFF, StateBefore: 0x4 /* RENDER_TARGET */, StateAfter: 0 /* PRESENT */}
				_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&b2)))
				_ = comCall(h.CmdList, 9) // CommandList::Close
				lists := [1]uintptr{h.CmdList}
				_ = comCall(h.Queue, 10, 1, uintptr(unsafe.Pointer(&lists[0])))
				h.waitGPU()
				return
			}
			b2 := d3d12ResourceBarrier{Resource: bb, Subresource: 0xFFFFFFFF, StateBefore: 0x4 /* RENDER_TARGET */, StateAfter: 0 /* PRESENT */}
				_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&b2)))
			}
		}

		hrClose := comCall(h.CmdList, 9) // Close
		if int32(hrClose) < 0 {
			slog.Error("Host D3D12 CmdList::Close échoué", "hr", fmt.Sprintf("0x%08X", uint32(hrClose)), "draws", len(draws), "dispatches", len(dispatches))
		}
		lists := [1]uintptr{h.CmdList}
		_ = comCall(h.Queue, 10, 1, uintptr(unsafe.Pointer(&lists[0]))) // ExecuteCommandLists
		hrPres := comCall(h.SwapChain, 8, uintptr(syncInterval), 0)     // Present
		if int32(hrPres) < 0 {
			h.dumpDebugMessages()
			slog.Error("Host D3D12 SwapChain::Present échoué", "hr", fmt.Sprintf("0x%08X", uint32(hrPres)), "removedReason", fmt.Sprintf("0x%08X", h.DeviceRemovedReason()))
		}
		h.waitGPU()
		if h.ReadbackBuf != 0 && h.ReadbackPtr != 0 && (h.introFrameCount == 1 || h.introFrameCount == 90) && h.introReached {
			h.saveReadbackPNG(fmt.Sprintf("%s\\astral_intro_%d.png", os.TempDir(), h.introFrameCount))
		}
		return
	}

	_ = comCall(h.SwapChain, 8, uintptr(syncInterval), 0)
}

func (h *HostD3D12) saveReadbackPNG(path string) {
	if h.ReadbackPtr == 0 || h.Width == 0 || h.Height == 0 || h.ReadbackPitch == 0 {
		return
	}
	w, ht := int(h.Width), int(h.Height)
	pitch := int(h.ReadbackPitch)
	src := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&h.ReadbackPtr))), pitch*ht)
	img := image.NewRGBA(image.Rect(0, 0, w, ht))
	nonBlack := 0
	nonBlue := 0
	var minR, maxR, minG, maxG, minB, maxB byte = 255, 0, 255, 0, 255, 0
	for y := 0; y < ht; y++ {
		sRow := src[y*pitch : y*pitch+w*4]
		dRow := img.Pix[y*img.Stride : y*img.Stride+w*4]
		copy(dRow, sRow)
		for x := 0; x < w*4; x += 4 {
			r, g, b := sRow[x], sRow[x+1], sRow[x+2]
			dRow[x+3] = 255
			if r > 4 || g > 4 || b > 4 {
				nonBlack++
			}
			if r > 40 || g > 40 || b > 60 {
				nonBlue++
			}
			if r < minR {
				minR = r
			}
			if r > maxR {
				maxR = r
			}
			if g < minG {
				minG = g
			}
			if g > maxG {
				maxG = g
			}
			if b < minB {
				minB = b
			}
			if b > maxB {
				maxB = b
			}
		}
	}
	f, err := os.Create(path)
	if err == nil {
		_ = png.Encode(f, img)
		_ = f.Close()
	}
	slog.Info("ASTRAL_SCREENSHOT sauvegardée",
		"path", path,
		"w", w, "h", ht,
		"nonBlack", nonBlack,
		"bright", nonBlue,
		"rgbMin", fmt.Sprintf("%d,%d,%d", minR, minG, minB),
		"rgbMax", fmt.Sprintf("%d,%d,%d", maxR, maxG, maxB),
	)
}

func (h *HostD3D12) dumpDebugMessages() {
	if h == nil || h.Device == 0 || os.Getenv("ASTRAL_D3D12_DEBUG") != "1" {
		return
	}
	var iq uintptr
	if int32(comCall(h.Device, 0, uintptr(unsafe.Pointer(&guidID3D12InfoQueue[0])), uintptr(unsafe.Pointer(&iq)))) < 0 || iq == 0 {
		return
	}
	defer comCall(iq, 2) // Release
	// ID3D12InfoQueue : ClearStoredMessages=4, GetMessage=5, GetNumStoredMessages=8
	n := uint64(comCall(iq, 8))
	if n > 12 {
		n = 12
	}
	for i := uint64(0); i < n; i++ {
		var sz uintptr
		_ = comCall(iq, 5, uintptr(i), 0, uintptr(unsafe.Pointer(&sz)))
		if sz == 0 || sz > 16384 {
			continue
		}
		buf := make([]byte, sz)
		if int32(comCall(iq, 5, uintptr(i), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&sz)))) >= 0 && len(buf) >= 32 {
			// D3D12_MESSAGE : { Category u32 (+0), Severity u32 (+4), ID u32 (+8), _pad u32 (+12), pDescription ptr (+16), DescriptionByteLength size_t (+24) }
			pDesc := *(*uintptr)(unsafe.Pointer(&buf[16]))
			dLen := *(*uintptr)(unsafe.Pointer(&buf[24]))
			if pDesc != 0 && dLen > 0 && dLen < 4096 {
				msg := string(unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&pDesc))), dLen))
				slog.Error("D3D12_DEBUG", "msg", msg)
			}
		}
	}
	_ = comCall(iq, 4) // ClearStoredMessages
}

// Present présente la trame courante sur la SwapChain DXGI hôte.
func (h *HostD3D12) Present(syncInterval uint32) {
	h.PresentClear(syncInterval, [4]float32{0.04, 0.05, 0.09, 1.0}, 0, nil, 0, 0, 0, nil)
}

var procGetForegroundWin = syscall.NewLazyDLL("user32.dll").NewProc("GetForegroundWindow")

// IsWindowFocused retourne true si la fenêtre Astral est actuellement au premier plan (focus Windows).
func IsWindowFocused() bool {
	hostMu.Lock()
	h := hostInstance
	hostMu.Unlock()
	if h == nil || h.HWND == 0 {
		return true
	}
	fg, _, _ := procGetForegroundWin.Call()
	return fg == h.HWND
}

// PresentHost présente la trame courante sur l'instance globale HostD3D12 si active.
func PresentHost(syncInterval uint32) {
	hostMu.Lock()
	h := hostInstance
	hostMu.Unlock()
	if h != nil {
		h.Present(syncInterval)
	}
}

// PresentHostFrame présente une trame avec couleur de clear, adresse du backbuffer invité, framebuffer optionnel, Dispatches Compute et DrawInstanced.
func PresentHostFrame(syncInterval uint32, clearColor [4]float32, presentVA uint64, srcRGBA []byte, srcW, srcH, srcPitch uint32, dispatches []HostDispatchPass, draws ...HostDrawPass) {
	hostMu.Lock()
	h := hostInstance
	hostMu.Unlock()
	if h != nil {
		h.PresentClear(syncInterval, clearColor, presentVA, srcRGBA, srcW, srcH, srcPitch, dispatches, draws...)
	}
}
