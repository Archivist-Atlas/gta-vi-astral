//go:build windows

package hle

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// d3d12xState centralise les objets COM Xbox Series D3D12.X créés pendant le boot.
type d3d12xState struct {
	mu            sync.Mutex
	deviceObj     uintptr
	nextHeapBase  atomic.Uint64
	nextGPUAddr   atomic.Uint64
	fenceValues   map[uintptr]*atomic.Uint64
	heapBases     map[uintptr]uint64
	resGPUAddrs   map[uintptr]uint64
	queuesCreated atomic.Int64
	listsCreated  atomic.Int64
	fencesCreated atomic.Int64
	heapsCreated  atomic.Int64
	resCreated    atomic.Int64
	psoCreated    atomic.Int64
}

func newD3D12XDevice() uintptr {
	st := &d3d12xState{
		fenceValues: make(map[uintptr]*atomic.Uint64),
		heapBases:   make(map[uintptr]uint64),
		resGPUAddrs: make(map[uintptr]uint64),
	}
	st.nextHeapBase.Store(0x0000100000000000)
	st.nextGPUAddr.Store(0x0000200000000000)

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
			if index != 0 {
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

	// Helpers de création d'objets enfants D3D12.X
	newQueue := func() uintptr {
		st.queuesCreated.Add(1)
		return NewCOMObjectWithHeader("ID3D12CommandQueueX", 48, 0x40, map[int]uintptr{
			3:  cbOK, // GetPrivateData
			4:  cbOK, // SetPrivateData
			5:  cbOK, // SetPrivateDataInterface
			6:  cbOK, // SetName
			7:  cbGetDevice,
			10: cbOK, // ExecuteCommandLists
			14: syscall.NewCallback(func(this, fenceObj, val uintptr) uintptr { // Signal
				st.mu.Lock()
				fv := st.fenceValues[fenceObj]
				st.mu.Unlock()
				if fv != nil {
					fv.Store(uint64(val))
				}
				return hresultOK
			}),
			15: cbOK, // Wait
			30: cbOK, // PresentX (slot primaire)
			32: cbOK, // PresentX (slot alternatif)
		})
	}

	newAllocator := func() uintptr {
		return NewCOMObject("ID3D12CommandAllocatorX", 16, map[int]uintptr{
			3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
			8: cbOK, // Reset
		})
	}

	// Vtable partagée pour toutes les ID3D12GraphicsCommandListX (GTA VI en crée 581+ au boot).
	// En-tête de 0x1000 octets mis à zéro : PutPointer==LimitPointer==0 ET protège jusqu'au-delà de +0x564.
	cmdListProto := NewCOMObjectWithHeader("ID3D12GraphicsCommandListX", 160, 0x1000, map[int]uintptr{
		3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
		8:   cbOK, // GetType
		9:   cbOK, // Close
		10:  cbOK, // Reset
		11:  cbOK, // ClearState
		131: cbOK, // 0x418 GPU sync (VIctory list.rs)
		132: cbOK, // 0x420 GPU sync
		139: cbOK, // 0x458 ResetX
	})
	cmdListVtable := *(*uintptr)(osPtr(cmdListProto))

	newCommandList := func() uintptr {
		st.listsCreated.Add(1)
		mem, _, _ := k32VirtualAlloc.Call(0, 0x1000, 0x3000, 0x04 /* PAGE_READWRITE */)
		if mem != 0 {
			*(*uintptr)(osPtr(mem)) = cmdListVtable
		}
		return mem
	}

	newFence := func(initVal uint64) uintptr {
		st.fencesCreated.Add(1)
		val := &atomic.Uint64{}
		val.Store(initVal)
		var obj uintptr
		obj = NewCOMObject("ID3D12FenceX", 16, map[int]uintptr{
			3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
			8: syscall.NewCallback(func(this uintptr) uintptr { // GetCompletedValue
				st.mu.Lock()
				fv := st.fenceValues[this]
				st.mu.Unlock()
				if fv != nil {
					return uintptr(fv.Load())
				}
				return 0
			}),
			9: cbOK, // SetEventOnCompletion
			10: syscall.NewCallback(func(this, v uintptr) uintptr { // Signal
				st.mu.Lock()
				fv := st.fenceValues[this]
				st.mu.Unlock()
				if fv != nil {
					fv.Store(uint64(v))
				}
				return hresultOK
			}),
		})
		st.mu.Lock()
		st.fenceValues[obj] = val
		st.mu.Unlock()
		return obj
	}

	newDescriptorHeap := func(descPtr uintptr) uintptr {
		st.heapsCreated.Add(1)
		// D3D12_DESCRIPTOR_HEAP_DESC : { Type u32, NumDescriptors u32, Flags u32, NodeMask u32 }
		numDescs := uintptr(4096)
		stride := uintptr(128)
		if descPtr != 0 {
			heapType := *(*uint32)(osPtr(descPtr))
			n := *(*uint32)(osPtr(descPtr + 4))
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
		baseAddr, _, _ := k32VirtualAlloc.Call(0, bytes, 0x3000 /* COMMIT|RESERVE */, 0x04 /* PAGE_READWRITE */)
		if baseAddr == 0 {
			baseAddr = uintptr(st.nextHeapBase.Add(0x1000000))
		}
		var obj uintptr
		obj = NewCOMObject("ID3D12DescriptorHeapX", 16, map[int]uintptr{
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
		st.mu.Lock()
		st.heapBases[obj] = uint64(baseAddr)
		st.mu.Unlock()
		return obj
	}

	newResource := func(gpuVA uint64) uintptr {
		st.resCreated.Add(1)
		if gpuVA == 0 {
			gpuVA = st.nextGPUAddr.Add(0x100000)
		}
		obj := NewCOMObject("ID3D12ResourceX", 24, map[int]uintptr{
			3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
			8:  cbOK, // Map
			9:  cbOK, // Unmap
			11: syscall.NewCallback(func(this uintptr) uintptr { // GetGPUVirtualAddress
				st.mu.Lock()
				va := st.resGPUAddrs[this]
				st.mu.Unlock()
				return uintptr(va)
			}),
		})
		st.mu.Lock()
		st.resGPUAddrs[obj] = gpuVA
		st.mu.Unlock()
		return obj
	}

	newGenericChild := func(area string) uintptr {
		return NewCOMObject(area, 24, map[int]uintptr{
			3: cbOK, 4: cbOK, 5: cbOK, 6: cbOK, 7: cbGetDevice,
		})
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
	cbCreatePSO := syscall.NewCallback(func(this, desc, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreatePSO")
		st.psoCreated.Add(1)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newGenericChild("ID3D12PipelineStateX")
		}
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
		switch heapType {
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
	cbAllocInfo := syscall.NewCallback(func(a0, a1, a2, a3, a4 uintptr) uintptr {
		outPtr := a1
		if a0 != st.deviceObj && a0 != 0 {
			outPtr = a0
		}
		if outPtr != 0 {
			words := unsafe.Slice((*uint64)(osPtr(outPtr)), 2)
			words[0] = 65536 // SizeInBytes
			words[1] = 65536 // Alignment
		}
		return outPtr
	})
	cbCreateCommitted := syscall.NewCallback(func(this, heapProps, heapFlags, desc, state, clearVal, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreateCommittedResource")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newResource(0)
		}
		return hresultOK
	})
	cbCreatePlaced := syscall.NewCallback(func(this, heap, offset, desc, state, clearVal, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreatePlacedResource")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newResource(0)
		}
		return hresultOK
	})
	cbCreatePlacedAtAddr := syscall.NewCallback(func(this, gpuAddr, desc, state, clearVal, iid, outPtr uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CreatePlacedResourceX", "gpuAddr", gpuAddr)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newResource(uint64(gpuAddr))
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

	cbCheckFeatureSupport := syscall.NewCallback(func(this, feature, dataPtr, dataSize uintptr) uintptr {
		slog.Debug("D3D12DeviceX!CheckFeatureSupport", "feature", feature, "size", dataSize)
		if dataPtr == 0 {
			return hresultInvalidArg
		}
		switch feature {
		case 0: // D3D12_FEATURE_D3D12_OPTIONS (64 bytes = 16 u32)
			if dataSize == 64 {
				opts := [16]uint32{
					1, 1, 0, 3, 3, 1, 1, 1,
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
		11:  cbCreatePSO,
		12:  cbCreateList,
		13:  cbCheckFeatureSupport,
		14:  cbCreateDescHeap,
		15:  cbDescIncrSize,
		16:  cbCreateRootSig,
		17:  cbOK, // CreateCBV
		18:  cbOK, // CreateSRV
		19:  cbOK, // CreateUAV
		20:  cbOK, // CreateRTV
		21:  cbOK, // CreateDSV
		22:  cbOK, // CreateSampler
		23:  cbOK, // CopyDescriptors
		24:  cbOK, // CopyDescriptorsSimple
		25:  cbAllocInfo,
		26:  cbCustomHeapProps,
		27:  cbCreateCommitted,
		28:  cbCreateHeap,
		29:  cbCreatePlaced,
		36:  cbCreateFence,
		38:  cbOK, // GetCopyableFootprints
		39:  cbCreateQueryHeap,
		41:  cbCreateCmdSig,
		48:  cbCreateQueue, // CreateCommandQueueX
		61:  cbOK,          // CreateSampler2
		62:  cbCreatePSO,   // CreateComputePipelineStateX
		69:  cbOK,          // Debug filter
		70:  cbOK,          // CreatePlacedSRV
		71:  cbOK,          // CreatePlacedUAV
		73:  cbCreateListX, // CreateCommandListX
		74:  cbOK,          // RegisterPagePool
		75:  cbOK,          // UnregisterPagePool
		76:  cbCreateCommitted,
		78:  cbOK, // SetFrameIntervalX
		79:  cbOK, // ScheduleFrameEventX
		80:  cbOK, // WaitFrameEventX
		81:  cbOK, // GetFrameStatisticsX
		86:  cbCreatePSO,
		108: cbAllocInfo,
		118: cbCreateShaderCacheSession,
		127: cbCreateCommitted,
		133: cbCreatePlacedAtAddr,
	})

	// Active le drapeau out_of_line à +0x18 (vérifié par VIctory)
	if st.deviceObj != 0 {
		*(*uint32)(osPtr(st.deviceObj + 0x18)) = 1
	}

	slog.Debug("ID3D12DeviceX initialisé (en-tête 0x58, out_of_line=1, 160 slots)")
	return st.deviceObj
}
