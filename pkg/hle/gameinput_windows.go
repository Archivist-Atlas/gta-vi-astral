//go:build windows

package hle

import (
	"log/slog"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	modUser32GI       = syscall.NewLazyDLL("user32.dll")
	procGetAsyncKey   = modUser32GI.NewProc("GetAsyncKeyState")
	modXInput14       = syscall.NewLazyDLL("xinput1_4.dll")
	procXInputGetStat = modXInput14.NewProc("XInputGetState")
)

const (
	giKindGamepad      uint32 = 0x00020000
	giDeviceConnected  uint32 = 0x00000001
	giBtnMenu          uint32 = 0x00000001
	giBtnView          uint32 = 0x00000002
	giBtnA             uint32 = 0x00000004
	giBtnB             uint32 = 0x00000008
	giBtnX             uint32 = 0x00000010
	giBtnY             uint32 = 0x00000020
	giBtnDPadUp        uint32 = 0x00000040
	giBtnDPadDown      uint32 = 0x00000080
	giBtnDPadLeft      uint32 = 0x00000100
	giBtnDPadRight     uint32 = 0x00000200
	giBtnLeftShoulder  uint32 = 0x00000400
	giBtnRightShoulder uint32 = 0x00000800
	giBtnLeftThumb     uint32 = 0x00001000
	giBtnRightThumb    uint32 = 0x00002000
)

// gameInputGamepadState correspond à GameInputGamepadState (28 octets, ABI GDK).
type gameInputGamepadState struct {
	Buttons          uint32
	LeftTrigger      float32
	RightTrigger     float32
	LeftThumbstickX  float32
	LeftThumbstickY  float32
	RightThumbstickX float32
	RightThumbstickY float32
}

// xinputState correspond à XINPUT_STATE (16 octets).
type xinputState struct {
	PacketNumber uint32
	Buttons      uint16
	LeftTrigger  uint8
	RightTrigger uint8
	ThumbLX      int16
	ThumbLY      int16
	ThumbRX      int16
	ThumbRY      int16
}

func keyDown(vk uintptr) bool {
	r, _, _ := procGetAsyncKey.Call(vk)
	return (r & 0x8000) != 0
}

func normStick(v int16) float32 {
	if v > -4096 && v < 4096 {
		return 0
	}
	if v < 0 {
		return float32(v) / 32768.0
	}
	return float32(v) / 32767.0
}

// pollLiveGamepad fusionne la manette XInput 0 (si branchée) et le clavier Windows
// (WASD = stick gauche, flèches = D-Pad, Entrée/Espace = A, Échap = Menu, Retour = B).
func pollLiveGamepad() gameInputGamepadState {
	var st gameInputGamepadState

	// 1. Manette XInput réelle (xinput1_4.dll)
	if procXInputGetStat.Find() == nil {
		var xs xinputState
		r, _, _ := procXInputGetStat.Call(0, uintptr(unsafe.Pointer(&xs)))
		if r == 0 {
			if xs.Buttons&0x0001 != 0 {
				st.Buttons |= giBtnDPadUp
			}
			if xs.Buttons&0x0002 != 0 {
				st.Buttons |= giBtnDPadDown
			}
			if xs.Buttons&0x0004 != 0 {
				st.Buttons |= giBtnDPadLeft
			}
			if xs.Buttons&0x0008 != 0 {
				st.Buttons |= giBtnDPadRight
			}
			if xs.Buttons&0x0010 != 0 {
				st.Buttons |= giBtnMenu
			}
			if xs.Buttons&0x0020 != 0 {
				st.Buttons |= giBtnView
			}
			if xs.Buttons&0x0040 != 0 {
				st.Buttons |= giBtnLeftThumb
			}
			if xs.Buttons&0x0080 != 0 {
				st.Buttons |= giBtnRightThumb
			}
			if xs.Buttons&0x0100 != 0 {
				st.Buttons |= giBtnLeftShoulder
			}
			if xs.Buttons&0x0200 != 0 {
				st.Buttons |= giBtnRightShoulder
			}
			if xs.Buttons&0x1000 != 0 {
				st.Buttons |= giBtnA
			}
			if xs.Buttons&0x2000 != 0 {
				st.Buttons |= giBtnB
			}
			if xs.Buttons&0x4000 != 0 {
				st.Buttons |= giBtnX
			}
			if xs.Buttons&0x8000 != 0 {
				st.Buttons |= giBtnY
			}
			st.LeftTrigger = float32(xs.LeftTrigger) / 255.0
			st.RightTrigger = float32(xs.RightTrigger) / 255.0
			st.LeftThumbstickX = normStick(xs.ThumbLX)
			st.LeftThumbstickY = normStick(xs.ThumbLY)
			st.RightThumbstickX = normStick(xs.ThumbRX)
			st.RightThumbstickY = normStick(xs.ThumbRY)
		}
	}

	// 2. Clavier Windows (mapping ergonomique de base)
	if keyDown(0x0D) || keyDown(0x20) { // Enter / Space -> A
		st.Buttons |= giBtnA
	}
	if keyDown(0x08) { // Backspace -> B
		st.Buttons |= giBtnB
	}
	if keyDown(0x1B) { // Escape -> Menu (Start)
		st.Buttons |= giBtnMenu
	}
	if keyDown(0x09) { // Tab -> View (Back)
		st.Buttons |= giBtnView
	}
	if keyDown(0x26) { // Up
		st.Buttons |= giBtnDPadUp
	}
	if keyDown(0x28) { // Down
		st.Buttons |= giBtnDPadDown
	}
	if keyDown(0x25) { // Left
		st.Buttons |= giBtnDPadLeft
	}
	if keyDown(0x27) { // Right
		st.Buttons |= giBtnDPadRight
	}
	if keyDown('W') {
		st.LeftThumbstickY = 1.0
	} else if keyDown('S') {
		st.LeftThumbstickY = -1.0
	}
	if keyDown('D') {
		st.LeftThumbstickX = 1.0
	} else if keyDown('A') {
		st.LeftThumbstickX = -1.0
	}
	if keyDown('R') {
		st.Buttons |= giBtnX
	}
	if keyDown('F') {
		st.Buttons |= giBtnY
	}
	if keyDown('Q') {
		st.Buttons |= giBtnLeftShoulder
	}
	if keyDown('E') {
		st.Buttons |= giBtnRightShoulder
	}
	return st
}

// bindGameInput branche un objet COM IGameInput / IGameInputReading / IGameInputDevice
// connecté au clavier Windows et à XInput (manette Xbox/PC).
func bindGameInput(t *LiveTable) {
	startTime := time.Now()
	var seqNum atomic.Uint64
	var nextToken atomic.Uint64
	nextToken.Store(1)

	nowUS := func() uint64 {
		return uint64(time.Since(startTime).Microseconds()) + 1000
	}

	// Allocation d'une structure GameInputDeviceInfo (256 octets) persistante
	devInfoMem, _, _ := k32VirtualAlloc.Call(0, 256, 0x3000, 0x04)
	if devInfoMem != 0 {
		*(*uint32)(osPtr(devInfoMem + 0)) = 256          // infoSize
		*(*uint16)(osPtr(devInfoMem + 4)) = 0x045E       // vendorId = Microsoft
		*(*uint16)(osPtr(devInfoMem + 6)) = 0x02EA       // productId = Xbox One S Controller
		*(*uint32)(osPtr(devInfoMem + 48)) = giKindGamepad // supportedInput
	}

	cbOK := syscall.NewCallback(func() uintptr { return hresultOK })
	cbTrue := syscall.NewCallback(func() uintptr { return 1 })
	cbNotFound := syscall.NewCallback(func() uintptr { return giReadingNotFound })

	// IGameInputDevice (16 slots)
	deviceObj := NewCOMObject("IGameInputDevice", 16, map[int]uintptr{
		3: syscall.NewCallback(func(this uintptr) uintptr { // GetDeviceInfo
			return devInfoMem
		}),
		4: syscall.NewCallback(func(this uintptr) uintptr { // GetDeviceStatus
			return uintptr(giDeviceConnected)
		}),
		5:  cbOK, // GetBatteryState
		12: cbOK, // SetRumbleState
	})

	// IGameInputReading (28 slots)
	readingObj := NewCOMObject("IGameInputReading", 28, map[int]uintptr{
		3: syscall.NewCallback(func(this uintptr) uintptr { // GetInputKind
			return uintptr(giKindGamepad)
		}),
		4: syscall.NewCallback(func(this, kind uintptr) uintptr { // GetSequenceNumber
			return uintptr(seqNum.Add(1))
		}),
		5: syscall.NewCallback(func(this uintptr) uintptr { // GetTimestamp
			return uintptr(nowUS())
		}),
		6: syscall.NewCallback(func(this, outDev uintptr) uintptr { // GetDevice
			if outDev >= 0x10000 {
				*(*uintptr)(osPtr(outDev)) = deviceObj
			}
			return 0
		}),
		16: syscall.NewCallback(func(this, outState uintptr) uintptr { // GetGamepadState
			if outState >= 0x10000 {
				*(*gameInputGamepadState)(osPtr(outState)) = pollLiveGamepad()
				return 1
			}
			return 0
		}),
	})

	cbGetTimestamp := syscall.NewCallback(func(this uintptr) uintptr {
		return uintptr(nowUS())
	})
	cbGetCurrentReading := syscall.NewCallback(func(this, inputKind, device, outReading uintptr) uintptr {
		kind := uint32(inputKind)
		if kind == 0 || kind&giKindGamepad != 0 {
			if outReading >= 0x10000 {
				*(*uintptr)(osPtr(outReading)) = readingObj
				return hresultOK
			}
		}
		return giReadingNotFound
	})
	cbRegReading := syscall.NewCallback(func(this, dev, kind, thresh, ctx, cb, outToken uintptr) uintptr {
		if outToken >= 0x10000 {
			*(*uint64)(osPtr(outToken)) = nextToken.Add(1)
		}
		return hresultOK
	})
	cbRegDevice := syscall.NewCallback(func(this, dev, kind, status, enumKind, ctx, cb, outToken uintptr) uintptr {
		if outToken >= 0x10000 {
			*(*uint64)(osPtr(outToken)) = nextToken.Add(1)
		}
		return hresultOK
	})
	cbRegSysBtn := syscall.NewCallback(func(this, dev, filter, ctx, cb, outToken uintptr) uintptr {
		if outToken >= 0x10000 {
			*(*uint64)(osPtr(outToken)) = nextToken.Add(1)
		}
		return hresultOK
	})
	cbReg5 := syscall.NewCallback(func(this, dev, ctx, cb, outToken uintptr) uintptr {
		if outToken >= 0x10000 {
			*(*uint64)(osPtr(outToken)) = nextToken.Add(1)
		}
		return hresultOK
	})

	gameInputObj := NewCOMObject("IGameInput", 22, map[int]uintptr{
		3:  cbGetTimestamp,      // GetCurrentTimestamp
		4:  cbGetCurrentReading, // GetCurrentReading (XInput + Clavier live)
		5:  cbNotFound,          // GetNextReading
		6:  cbNotFound,          // GetPreviousReading
		7:  cbNotFound,          // GetTemporalReading
		8:  cbRegReading,        // RegisterReadingCallback (7 args)
		9:  cbRegDevice,         // RegisterDeviceCallback (8 args)
		10: cbRegSysBtn,         // RegisterSystemButtonCallback (6 args)
		11: cbReg5,              // RegisterGuideButtonCallback (5 args)
		12: cbReg5,              // RegisterKeyboardLayoutCallback (5 args)
		13: cbOK,                // StopCallback
		14: cbTrue,              // UnregisterCallback
		16: cbOK,                // FindDeviceFromId
		17: cbOK,                // FindDeviceFromObject
		18: cbOK,                // FindDeviceFromPlatformHandle
		19: cbOK,                // FindDeviceFromPlatformString
		20: cbOK,                // EnableOemDeviceSupport
		21: cbOK,                // SetFocusPolicy
	})

	cbGameInputCreate := syscall.NewCallback(func(outPtr uintptr) uintptr {
		slog.Info("GameInput!GameInputCreate -> IGameInput COM actif (XInput + Clavier Windows)")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = gameInputObj
		}
		return hresultOK
	})
	t.bind("gameinput.dll", "#1", cbGameInputCreate)
	t.bind("gameinput.dll", "GameInputCreate", cbGameInputCreate)
}
