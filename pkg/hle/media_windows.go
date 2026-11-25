//go:build windows

package hle

import (
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// bindMediaFoundation intercepte MFCreateSourceResolver (MFPlat.DLL) afin de réécrire
// les chemins console (G:\, T:\, file:///G:/...) vers VirtualFS et bloquer les URL réseau
// en mode hors-ligne (conforme à VIctory/media.rs).
func bindMediaFoundation(t *LiveTable, vfs *VirtualFS) {
	mfplat := syscall.NewLazyDLL("mfplat.dll")
	procMFCreateSR := mfplat.NewProc("MFCreateSourceResolver")

	const mfUnsupportedScheme uintptr = 0xC00D36C3

	var (
		mu      sync.Mutex
		proxies = map[uintptr]uintptr{} // proxy -> host IMFSourceResolver*
	)

	comCallHost := func(obj uintptr, slot int, args ...uintptr) uintptr {
		if obj < 0x10000 {
			return hresultInvalidArg
		}
		vtbl := *(*uintptr)(osPtr(obj))
		if vtbl < 0x10000 {
			return hresultInvalidArg
		}
		fn := *(*uintptr)(osPtr(vtbl + uintptr(slot)*8))
		if fn < 0x10000 {
			return hresultNotImpl
		}
		all := make([]uintptr, 0, len(args)+1)
		all = append(all, obj)
		all = append(all, args...)
		r, _, _ := syscall.SyscallN(fn, all...)
		return r
	}

	rewriteMFURL := func(urlPtr uintptr) (*uint16, bool, bool) {
		if urlPtr < 0x10000 {
			return nil, false, false
		}
		raw := readUTF16Str(urlPtr)
		low := strings.ToLower(raw)
		if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "rtsp://") {
			slog.Info("MFSourceResolver URL réseau bloquée (hors-ligne)", "url", raw)
			return nil, false, true
		}
		path := raw
		if strings.HasPrefix(low, "file:///") {
			path = strings.ReplaceAll(raw[8:], "/", `\`)
		}
		if vfs != nil {
			if mapped, ok := vfs.ResolveRead(path); ok {
				if p, err := syscall.UTF16PtrFromString(mapped); err == nil {
					slog.Info("MFSourceResolver réécrit", "orig", raw, "host", mapped)
					return p, true, false
				}
			}
		}
		return nil, false, false
	}

	getHost := func(this uintptr) uintptr {
		mu.Lock()
		h := proxies[this]
		mu.Unlock()
		return h
	}

	srProto := NewCOMObject("IMFSourceResolver", 10, map[int]uintptr{
		3: syscall.NewCallback(func(this, urlPtr, flags, props, objTypeOut, objOut uintptr) uintptr { // CreateObjectFromURL
			host := getHost(this)
			mapped, ok, blocked := rewriteMFURL(urlPtr)
			if blocked {
				return mfUnsupportedScheme
			}
			if ok {
				urlPtr = uintptr(unsafe.Pointer(mapped))
			}
			return comCallHost(host, 3, urlPtr, flags, props, objTypeOut, objOut)
		}),
		4: syscall.NewCallback(func(this, stream, urlPtr, flags, props, objTypeOut, objOut uintptr) uintptr { // CreateObjectFromByteStream
			return comCallHost(getHost(this), 4, stream, urlPtr, flags, props, objTypeOut, objOut)
		}),
		5: syscall.NewCallback(func(this, urlPtr, flags, props, cancelOut, cb, state uintptr) uintptr { // BeginCreateObjectFromURL
			host := getHost(this)
			mapped, ok, blocked := rewriteMFURL(urlPtr)
			if blocked {
				return mfUnsupportedScheme
			}
			if ok {
				urlPtr = uintptr(unsafe.Pointer(mapped))
			}
			return comCallHost(host, 5, urlPtr, flags, props, cancelOut, cb, state)
		}),
		6: syscall.NewCallback(func(this, res, objTypeOut, objOut uintptr) uintptr { // EndCreateObjectFromURL
			return comCallHost(getHost(this), 6, res, objTypeOut, objOut)
		}),
		7: syscall.NewCallback(func(this, stream, urlPtr, flags, props, cancelOut, cb, state uintptr) uintptr { // BeginCreateObjectFromByteStream
			return comCallHost(getHost(this), 7, stream, urlPtr, flags, props, cancelOut, cb, state)
		}),
		8: syscall.NewCallback(func(this, res, objTypeOut, objOut uintptr) uintptr { // EndCreateObjectFromByteStream
			return comCallHost(getHost(this), 8, res, objTypeOut, objOut)
		}),
		9: syscall.NewCallback(func(this, cookie uintptr) uintptr { // CancelObjectCreation
			return comCallHost(getHost(this), 9, cookie)
		}),
	})
	srVtable := *(*uintptr)(osPtr(srProto))

	cbMFCreateSR := syscall.NewCallback(func(outPtr uintptr) uintptr {
		if outPtr < 0x10000 || procMFCreateSR.Find() != nil {
			return hresultInvalidArg
		}
		var hostSR uintptr
		hr, _, _ := procMFCreateSR.Call(uintptr(unsafe.Pointer(&hostSR)))
		if int32(hr) < 0 || hostSR == 0 {
			return hr
		}
		proxy, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if proxy == 0 {
			*(*uintptr)(osPtr(outPtr)) = hostSR
			return hresultOK
		}
		*(*uintptr)(osPtr(proxy)) = srVtable
		mu.Lock()
		proxies[proxy] = hostSR
		mu.Unlock()
		*(*uintptr)(osPtr(outPtr)) = proxy
		slog.Info("MFPlat!MFCreateSourceResolver -> proxy VirtualFS actif")
		return hresultOK
	})
	t.bind("mfplat.dll", "MFCreateSourceResolver", cbMFCreateSR)
}
