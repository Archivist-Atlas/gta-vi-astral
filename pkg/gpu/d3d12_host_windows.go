//go:build windows

package gpu

import (
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"unsafe"
)

var (
	modD3D12Host            = syscall.NewLazyDLL("d3d12.dll")
	modDXGIHost             = syscall.NewLazyDLL("dxgi.dll")
	procD3D12CreateDevice   = modD3D12Host.NewProc("D3D12CreateDevice")
	procCreateDXGIFactory2  = modDXGIHost.NewProc("CreateDXGIFactory2")

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
	// IID_IDXGIFactory4 = {1bc6ea02-ef36-464f-bf0c-21ca39e5168a}
	guidIDXGIFactory4 = [16]byte{
		0x02, 0xea, 0xc6, 0x1b, 0x36, 0xef, 0x4f, 0x46,
		0xbf, 0x0c, 0x21, 0xca, 0x39, 0xe5, 0x16, 0x8a,
	}
)

// HostD3D12 encapsule un vrai ID3D12Device Windows, une CommandQueue directe
// et une SwapChain DXGI attachée à la fenêtre Win32 ASTRAL.
type HostD3D12 struct {
	Device    uintptr
	Queue     uintptr
	Factory   uintptr
	SwapChain uintptr
	HWND      uintptr
	Width     uint32
	Height    uint32
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

// InitHostD3D12 initialise le vrai device D3D12 hôte Windows et attache une SwapChain DXGI à hwnd.
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

	var swapChain uintptr
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
		hr = comCall(factory, 15, queue, hwnd, uintptr(unsafe.Pointer(&scDesc)), 0, 0, uintptr(unsafe.Pointer(&swapChain)))
		if int32(hr) < 0 {
			slog.Warn("CreateSwapChainForHwnd hôte échoué (poursuite sans swapchain)", "hr", fmt.Sprintf("0x%08X", uint32(hr)))
		}
	}

	hostInstance = &HostD3D12{
		Device:    device,
		Queue:     queue,
		Factory:   factory,
		SwapChain: swapChain,
		HWND:      hwnd,
		Width:     width,
		Height:    height,
	}
	slog.Info("Host D3D12 + SwapChain DXGI initialisés", "device", fmt.Sprintf("0x%X", device), "swapchain", fmt.Sprintf("0x%X", swapChain))
	return hostInstance, nil
}

// Present présente la trame courante sur la SwapChain DXGI hôte (slot 8 = IDXGISwapChain::Present).
func (h *HostD3D12) Present(syncInterval uint32) {
	if h == nil || h.SwapChain == 0 {
		return
	}
	_ = comCall(h.SwapChain, 8, uintptr(syncInterval), 0)
}
