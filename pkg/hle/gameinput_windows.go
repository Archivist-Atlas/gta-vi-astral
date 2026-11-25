//go:build windows

package hle

import (
	"fmt"
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
	modWinMM          = syscall.NewLazyDLL("winmm.dll")
	procJoyGetPosEx   = modWinMM.NewProc("joyGetPosEx")
	procJoyGetDevCaps = modWinMM.NewProc("joyGetDevCapsW")
)

const (
	giKindGamepad      uint32 = 0x00040000 // GameInputKindGamepad (1 << 18 = 262144)
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

// joyInfoEx correspond à JOYINFOEX (52 octets, winmm.dll) pour DualSense/PS5, DualShock, Switch Pro et manettes HID génériques.
type joyInfoEx struct {
	Size         uint32
	Flags        uint32
	Xpos         uint32
	Ypos         uint32
	Zpos         uint32
	Rpos         uint32
	Upos         uint32
	Vpos         uint32
	Buttons      uint32
	ButtonNumber uint32
	POV          uint32
	Reserved1    uint32
	Reserved2    uint32
}

// joyCapsW correspond au début de JOYCAPSW (winmm.dll) pour identifier VID/PID (Sony 0x054C, Nintendo 0x057E, etc.).
type joyCapsW struct {
	Mid  uint16
	Pid  uint16
	Name [32]uint16
	_pad [360]byte
}

var (
	giLoggedController atomic.Bool
	giLoggedFirstBtn   atomic.Bool
)

func normU16Axis(v uint32, invert bool) float32 {
	f := (float32(v) - 32767.5) / 32767.5
	if f > -0.12 && f < 0.12 {
		return 0
	}
	if f < -1.0 {
		f = -1.0
	} else if f > 1.0 {
		f = 1.0
	}
	if invert {
		return -f
	}
	return f
}

// pollLiveGamepad fusionne toutes les manettes connectées :
// 1) XInput slots 0..3 (Xbox 360 / One / Series / PC XInput)
// 2) winmm.dll HID/DirectInput slots 0..15 (DualSense PS5, DualShock 4, Switch Pro, manettes USB/BT génériques)
// 3) Clavier Windows (WASD = stick gauche, flèches = D-Pad, Entrée/Espace = A, Retour = B, Y/F = Y, R = X, Échap = Menu).
func pollLiveGamepad() gameInputGamepadState {
	var st gameInputGamepadState
	xinputFound := false

	// 1. Manettes XInput réelles (slots 0..3)
	if procXInputGetStat.Find() == nil {
		for slot := uintptr(0); slot < 4; slot++ {
			var xs xinputState
			r, _, _ := procXInputGetStat.Call(slot, uintptr(unsafe.Pointer(&xs)))
			if r != 0 {
				continue
			}
			xinputFound = true
			if giLoggedController.CompareAndSwap(false, true) {
				slog.Info("GameInput manette XInput détectée", "slot", slot)
			}
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
			break
		}
	}

	// 2. Manettes HID / DirectInput universelles (DualSense PS5, DualShock 4, Switch Pro, génériques) via winmm.dll
	if !xinputFound && procJoyGetPosEx.Find() == nil {
		for id := uintptr(0); id < 16; id++ {
			ji := joyInfoEx{Size: uint32(unsafe.Sizeof(joyInfoEx{})), Flags: 0xFF /* JOY_RETURNALL */}
			r, _, _ := procJoyGetPosEx.Call(id, uintptr(unsafe.Pointer(&ji)))
			if r != 0 {
				continue
			}
			var caps joyCapsW
			isSony := false
			if procJoyGetDevCaps.Find() == nil {
				if rc, _, _ := procJoyGetDevCaps.Call(id, uintptr(unsafe.Pointer(&caps)), uintptr(unsafe.Sizeof(caps))); rc == 0 {
					isSony = caps.Mid == 0x054C // Sony Interactive Entertainment (DualSense / DualShock 4)
				}
			}
			if giLoggedController.CompareAndSwap(false, true) {
				kindName := "HID générique"
				if isSony {
					kindName = "PlayStation DualSense/DualShock"
				}
				slog.Info("GameInput manette universelle détectée (winmm)",
					"type", kindName,
					"slot", id,
					"vid", fmt.Sprintf("0x%04X", caps.Mid),
					"pid", fmt.Sprintf("0x%04X", caps.Pid),
				)
			}

			// Croix directionnelle (POV en centièmes de degré, 0xFFFF = centré)
			if ji.POV < 36000 {
				if ji.POV <= 4500 || ji.POV >= 31500 {
					st.Buttons |= giBtnDPadUp
				}
				if ji.POV >= 4500 && ji.POV <= 13500 {
					st.Buttons |= giBtnDPadRight
				}
				if ji.POV >= 13500 && ji.POV <= 22500 {
					st.Buttons |= giBtnDPadDown
				}
				if ji.POV >= 22500 && ji.POV <= 31500 {
					st.Buttons |= giBtnDPadLeft
				}
			}

			b := ji.Buttons
			if isSony {
				// Mapping natif DualSense (PS5) / DualShock 4 (PS4) en HID Windows :
				// 0=Carré(X), 1=Croix(A), 2=Rond(B), 3=Triangle(Y), 4=L1, 5=R1, 6=L2, 7=R2, 8=Create, 9=Options, 10=L3, 11=R3, 13=Touchpad
				if b&(1<<0) != 0 {
					st.Buttons |= giBtnX
				}
				if b&(1<<1) != 0 {
					st.Buttons |= giBtnA
				}
				if b&(1<<2) != 0 {
					st.Buttons |= giBtnB
				}
				if b&(1<<3) != 0 {
					st.Buttons |= giBtnY
				}
				if b&(1<<4) != 0 {
					st.Buttons |= giBtnLeftShoulder
				}
				if b&(1<<5) != 0 {
					st.Buttons |= giBtnRightShoulder
				}
				if b&(1<<8) != 0 || b&(1<<13) != 0 {
					st.Buttons |= giBtnView
				}
				if b&(1<<9) != 0 {
					st.Buttons |= giBtnMenu
				}
				if b&(1<<10) != 0 {
					st.Buttons |= giBtnLeftThumb
				}
				if b&(1<<11) != 0 {
					st.Buttons |= giBtnRightThumb
				}
				st.LeftThumbstickX = normU16Axis(ji.Xpos, false)
				st.LeftThumbstickY = normU16Axis(ji.Ypos, true)
				st.RightThumbstickX = normU16Axis(ji.Zpos, false)
				st.RightThumbstickY = normU16Axis(ji.Rpos, true)
				st.LeftTrigger = float32(ji.Upos) / 65535.0
				st.RightTrigger = float32(ji.Vpos) / 65535.0
				if st.LeftTrigger < 0.05 && b&(1<<6) != 0 {
					st.LeftTrigger = 1.0
				}
				if st.RightTrigger < 0.05 && b&(1<<7) != 0 {
					st.RightTrigger = 1.0
				}
			} else {
				// Mapping HID générique (Switch Pro, 8BitDo, manettes PC DirectInput)
				if b&(1<<0) != 0 {
					st.Buttons |= giBtnA
				}
				if b&(1<<1) != 0 {
					st.Buttons |= giBtnB
				}
				if b&(1<<2) != 0 {
					st.Buttons |= giBtnX
				}
				if b&(1<<3) != 0 {
					st.Buttons |= giBtnY
				}
				if b&(1<<4) != 0 {
					st.Buttons |= giBtnLeftShoulder
				}
				if b&(1<<5) != 0 {
					st.Buttons |= giBtnRightShoulder
				}
				if b&(1<<6) != 0 {
					st.Buttons |= giBtnView
				}
				if b&(1<<7) != 0 {
					st.Buttons |= giBtnMenu
				}
				if b&(1<<8) != 0 {
					st.Buttons |= giBtnLeftThumb
				}
				if b&(1<<9) != 0 {
					st.Buttons |= giBtnRightThumb
				}
				st.LeftThumbstickX = normU16Axis(ji.Xpos, false)
				st.LeftThumbstickY = normU16Axis(ji.Ypos, true)
				st.RightThumbstickX = normU16Axis(ji.Zpos, false)
				st.RightThumbstickY = normU16Axis(ji.Rpos, true)
				st.LeftTrigger = float32(ji.Upos) / 65535.0
				st.RightTrigger = float32(ji.Vpos) / 65535.0
			}
			break
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
	if keyDown('F') || keyDown('Y') {
		st.Buttons |= giBtnY
	}
	if keyDown('Q') {
		st.Buttons |= giBtnLeftShoulder
	}
	if keyDown('E') {
		st.Buttons |= giBtnRightShoulder
	}
	if st.Buttons != 0 && giLoggedFirstBtn.CompareAndSwap(false, true) {
		slog.Info("GameInput premier appui bouton détecté", "buttons", fmt.Sprintf("0x%04X", st.Buttons))
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

	// Allocation d'une structure GameInputDeviceInfo (512 octets, ABI GDK exact)
	devInfoMem, _, _ := k32VirtualAlloc.Call(0, 512, 0x3000, 0x04)
	if devInfoMem != 0 {
		*(*uint32)(osPtr(devInfoMem + 0x00)) = 304           // infoSize
		*(*uint16)(osPtr(devInfoMem + 0x04)) = 0x045E        // vendorId = Microsoft
		*(*uint16)(osPtr(devInfoMem + 0x06)) = 0x0B12        // productId = Xbox Series X|S Controller
		*(*uint32)(osPtr(devInfoMem + 0x20)) = 0x41535452    // deviceId[0..3] non nul
		*(*uint32)(osPtr(devInfoMem + 0x40)) = 0x41535452    // deviceRootId[0..3] non nul
		*(*uint32)(osPtr(devInfoMem + 0x60)) = 3             // deviceFamily = GameInputFamilyXboxOne
		*(*uint32)(osPtr(devInfoMem + 0x68)) = giKindGamepad // supportedInput (offset +0x68!)
		*(*uint32)(osPtr(devInfoMem + 0x6C)) = 0x0F          // supportedRumbleMotors
		*(*uint32)(osPtr(devInfoMem + 0x7C)) = 6             // controllerAxisCount
		*(*uint32)(osPtr(devInfoMem + 0x80)) = 14            // controllerButtonCount
		*(*uint32)(osPtr(devInfoMem + 0x84)) = 1             // controllerSwitchCount
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

	var gamepadPolls atomic.Uint64
	cbGetGamepadState := syscall.NewCallback(func(this, outState uintptr) uintptr { // GetGamepadState (slot 22, 0xB0)
		if outState >= 0x10000 {
			st := pollLiveGamepad()
			*(*gameInputGamepadState)(osPtr(outState)) = st
			if n := gamepadPolls.Add(1); n == 1 {
				slog.Info("IGameInputReading!GetGamepadState actif (manette XInput + clavier Windows connectés)")
			}
			return 1
		}
		return 0
	})
	// IGameInputReading (28 slots, ABI GDK)
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
		22: cbGetGamepadState, // GetGamepadState (0xB0)
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
		tok := nextToken.Add(1)
		if outToken >= 0x10000 {
			*(*uint64)(osPtr(outToken)) = tok
		}
		// Si le titre demande l'énumération initiale des périphériques connectés (Async=1 ou Blocking=2)
		// et filtre les manettes (kind == 0 ou giKindGamepad), on notifie immédiatement notre manette virtuelle.
		if uint32(enumKind) != 0 && (uint32(kind) == 0 || uint32(kind)&giKindGamepad != 0) && cb >= 0x10000 && cb < 0x800000000000 {
			_, _, _ = syscall.SyscallN(cb, uintptr(tok), ctx, deviceObj, uintptr(nowUS()), uintptr(giDeviceConnected), 0)
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
