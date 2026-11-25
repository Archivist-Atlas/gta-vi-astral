//go:build windows

package hle

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
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
	giKindKeyboard     uint32 = 0x00000010 // GameInputKindKeyboard (1 << 4)
	giKindGamepad      uint32 = 0x00040000 // GameInputKindGamepad (1 << 18 = 262144)
	giDeviceConnected  uint32 = 0x00000003 // GameInputDeviceConnected (0x1) | GameInputDeviceInputEnabled (0x2)
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
					if caps.Mid == 0 && caps.Pid == 0 {
						continue // Périphérique virtuel/fantôme sans VID/PID réel
					}
					isSony = caps.Mid == 0x054C // Sony Interactive Entertainment (DualSense / DualShock 4)
				} else {
					continue
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
	var nextToken atomic.Uint64
	nextToken.Store(1)

	nowUS := func() uint64 {
		return uint64(time.Since(startTime).Microseconds()) + 1000
	}

	// Allocation d'une structure GameInputDeviceInfo (512 octets, ABI GDK exact : 320 octets)
	devInfoMem, _, _ := k32VirtualAlloc.Call(0, 512, 0x3000, 0x04)
	if devInfoMem != 0 {
		*(*uint32)(osPtr(devInfoMem + 0x00)) = 320           // infoSize (0x140)
		*(*uint16)(osPtr(devInfoMem + 0x04)) = 0x045E        // vendorId = Microsoft
		*(*uint16)(osPtr(devInfoMem + 0x06)) = 0x0B12        // productId = Xbox Series X|S Controller
		*(*uint32)(osPtr(devInfoMem + 0x20)) = 0x41535452    // deviceId[0..3] non nul
		*(*uint32)(osPtr(devInfoMem + 0x40)) = 0x41535452    // deviceRootId[0..3] non nul
		*(*uint32)(osPtr(devInfoMem + 0x60)) = 0             // deviceFamily = 0 (comme ScarlettPort)
		*(*uint32)(osPtr(devInfoMem + 0x68)) = giKindGamepad // supportedInput (offset +0x68!)
		*(*uint32)(osPtr(devInfoMem + 0x6C)) = 0x03          // supportedRumbleMotors (low + high)
	}

	cbOK := syscall.NewCallback(func() uintptr { return hresultOK })
	cbTrue := syscall.NewCallback(func() uintptr { return 1 })
	cbNotFound := syscall.NewCallback(func() uintptr { return giReadingNotFound })

	giTrace := os.Getenv("ASTRAL_GI_TRACE") == "1"
	var traceCounts [32]atomic.Int32
	logGI := func(idx int, name string, args ...any) {
		if giTrace && traceCounts[idx].Add(1) <= 3 {
			slog.Info("GI_TRACE "+name, args...)
		}
	}

	// IGameInputDevice (16 slots)
	deviceObj := NewCOMObject("IGameInputDevice", 16, map[int]uintptr{
		3: syscall.NewCallback(func(this uintptr) uintptr { // GetDeviceInfo
			logGI(0, "Device.GetDeviceInfo", "this", fmt.Sprintf("0x%X", this))
			return devInfoMem
		}),
		4: syscall.NewCallback(func(this uintptr) uintptr { // GetDeviceStatus
			logGI(1, "Device.GetDeviceStatus", "this", fmt.Sprintf("0x%X", this))
			return uintptr(giDeviceConnected)
		}),
		5:  cbOK, // GetBatteryState
		10: cbOK, // SetRumbleState / SetFeedbackMotor (GDK 2026)
		12: cbOK, // SetRumbleState (GDK 2020)
	})

	// Périphérique Clavier (GameInputKindKeyboard = 0x10)
	kbdInfoMem, _, _ := k32VirtualAlloc.Call(0, 512, 0x3000, 0x04)
	if kbdInfoMem != 0 {
		*(*uint32)(osPtr(kbdInfoMem + 0x00)) = 320
		*(*uint32)(osPtr(kbdInfoMem + 0x20)) = 0x4B424431 // "1DBK"
		*(*uint32)(osPtr(kbdInfoMem + 0x40)) = 0x4B424431
		*(*uint32)(osPtr(kbdInfoMem + 0x68)) = giKindKeyboard
	}
	kbdDeviceObj := NewCOMObject("IGameInputDevice_Kbd", 16, map[int]uintptr{
		3: syscall.NewCallback(func(this uintptr) uintptr { return kbdInfoMem }),
		4: syscall.NewCallback(func(this uintptr) uintptr { return uintptr(giDeviceConnected) }),
	})

	var gamepadPolls atomic.Uint64
	var btnDelivered atomic.Int32
	// Layout natif d'un objet IGameInputReading (64 octets en mémoire OS, GC-safe) :
	//   +0x00 : vtable (uintptr)
	//   +0x08 : seq (uint64)
	//   +0x10 : timestamp (uint64)
	//   +0x18 : state (gameInputGamepadState, 28 octets)
	cbGetGamepadState := syscall.NewCallback(func(this, outState uintptr) uintptr { // GetGamepadState (slot 22, 0xB0)
		if this >= 0x10000 && outState >= 0x10000 {
			st := *(*gameInputGamepadState)(osPtr(this + 0x18))
			*(*gameInputGamepadState)(osPtr(outState)) = st
			if n := gamepadPolls.Add(1); n == 1 {
				slog.Info("IGameInputReading!GetGamepadState actif (manette XInput/HID + clavier Windows connectés)")
			}
			if st.Buttons != 0 {
				if c := btnDelivered.Add(1); c <= 5 || c%60 == 0 {
					slog.Info("IGameInputReading!GetGamepadState -> bouton transmis au jeu", "n", c, "buttons", fmt.Sprintf("0x%04X", st.Buttons), "seq", *(*uint64)(osPtr(this + 0x08)))
				}
			}
			return 1
		}
		return 0
	})
	// Vtable partagée IGameInputReading (28 slots, ABI GDK) — Gamepad dédié (0x40000)
	readingProto := NewCOMObjectWithHeader("IGameInputReading", 28, 64, map[int]uintptr{
		3: syscall.NewCallback(func(this uintptr) uintptr { // GetInputKind
			logGI(2, "Reading.GetInputKind")
			return uintptr(giKindGamepad)
		}),
		4: syscall.NewCallback(func(this, kind uintptr) uintptr { // GetSequenceNumber
			logGI(3, "Reading.GetSequenceNumber", "kind", fmt.Sprintf("0x%X", uint32(kind)))
			if this >= 0x10000 && (uint32(kind) == 0 || uint32(kind)&giKindGamepad != 0) {
				return uintptr(*(*uint64)(osPtr(this + 0x08)))
			}
			return 0
		}),
		5: syscall.NewCallback(func(this uintptr) uintptr { // GetTimestamp
			logGI(4, "Reading.GetTimestamp")
			if this >= 0x10000 {
				return uintptr(*(*uint64)(osPtr(this + 0x10)))
			}
			return uintptr(nowUS())
		}),
		6: syscall.NewCallback(func(this, outDev uintptr) uintptr { // GetDevice
			logGI(5, "Reading.GetDevice")
			if outDev >= 0x10000 {
				*(*uintptr)(osPtr(outDev)) = deviceObj
			}
			return 0
		}),
		14: syscall.NewCallback(func(this uintptr) uintptr { return 0 }), // GetKeyCount (0 sur manette)
		15: syscall.NewCallback(func(this, cnt, out uintptr) uintptr { return 0 }), // GetKeyState
		22: cbGetGamepadState, // GetGamepadState (0xB0)
		24: syscall.NewCallback(func(this, outNav uintptr) uintptr { // GetUiNavigationState (0xC0)
			if this < 0x10000 || outNav < 0x10000 {
				return 0
			}
			st := *(*gameInputGamepadState)(osPtr(this + 0x18))
			var nav uint32
			if st.Buttons&giBtnMenu != 0 {
				nav |= 0x0001
			}
			if st.Buttons&giBtnView != 0 {
				nav |= 0x0002
			}
			if st.Buttons&giBtnA != 0 {
				nav |= 0x0004 // Accept
			}
			if st.Buttons&giBtnB != 0 {
				nav |= 0x0008 // Cancel
			}
			if st.Buttons&giBtnDPadUp != 0 || st.LeftThumbstickY > 0.5 {
				nav |= 0x0010
			}
			if st.Buttons&giBtnDPadDown != 0 || st.LeftThumbstickY < -0.5 {
				nav |= 0x0020
			}
			if st.Buttons&giBtnDPadLeft != 0 || st.LeftThumbstickX < -0.5 {
				nav |= 0x0040
			}
			if st.Buttons&giBtnDPadRight != 0 || st.LeftThumbstickX > 0.5 {
				nav |= 0x0080
			}
			if st.Buttons&giBtnX != 0 {
				nav |= 0x0100 // Context1
			}
			if st.Buttons&giBtnY != 0 {
				nav |= 0x0200 // Context2
			}
			*(*uint32)(osPtr(outNav)) = nav
			return 1
		}),
	})
	// Objet IGameInputReading dédié au clavier (0x10) pour ne pas interférer avec le verrouillage manette
	var kbdSeq atomic.Uint64
	kbdReadingObj := NewCOMObject("IGameInputReading_Kbd", 28, map[int]uintptr{
		3: syscall.NewCallback(func(this uintptr) uintptr { return uintptr(giKindKeyboard) }),
		4: syscall.NewCallback(func(this, kind uintptr) uintptr {
			if uint32(kind) == 0 || uint32(kind)&giKindKeyboard != 0 {
				return uintptr(kbdSeq.Load())
			}
			return 0
		}),
		5: syscall.NewCallback(func(this uintptr) uintptr { return uintptr(nowUS()) }),
		6: syscall.NewCallback(func(this, outDev uintptr) uintptr {
			if outDev >= 0x10000 {
				*(*uintptr)(osPtr(outDev)) = kbdDeviceObj
			}
			return 0
		}),
		14: syscall.NewCallback(func(this uintptr) uintptr { // GetKeyCount (0x70)
			var n uintptr
			for _, vk := range []uintptr{0x0D, 0x20, 0x1B, 0x08, 0x09, 0x25, 0x26, 0x27, 0x28, 'Y', 'X', 'A', 'B', 'W', 'S', 'D', 'F', 'E', 'Q', 'R'} {
				if keyDown(vk) {
					n++
				}
			}
			return n
		}),
		15: syscall.NewCallback(func(this, cnt, out uintptr) uintptr { // GetKeyState (0x78)
			if out < 0x10000 || cnt == 0 {
				return 0
			}
			type giKeyState struct {
				ScanCode   uint32
				CodePoint  uint32
				VirtualKey uint8
				IsDeadKey  uint8
				_pad       uint16
			}
			var written uintptr
			for _, vk := range []uintptr{0x0D, 0x20, 0x1B, 0x08, 0x09, 0x25, 0x26, 0x27, 0x28, 'Y', 'X', 'A', 'B', 'W', 'S', 'D', 'F', 'E', 'Q', 'R'} {
				if written >= cnt {
					break
				}
				if keyDown(vk) {
					*(*giKeyState)(osPtr(out + written*12)) = giKeyState{
						CodePoint:  uint32(vk),
						VirtualKey: uint8(vk),
					}
					written++
				}
			}
			return written
		}),
		22: syscall.NewCallback(func(this, outState uintptr) uintptr { return 0 }), // false sur clavier
	})
	readingVtable := *(*uintptr)(osPtr(readingProto))

	// Ring buffer de 64 objets IGameInputReading distincts en mémoire native (snapshots figés
	// pour que la détection de front montant cur.Buttons & ~prev.Buttons fonctionne à 100 %).
	const ringCap = 64
	ringMem, _, _ := k32VirtualAlloc.Call(0, ringCap*64, 0x3000, 0x04)
	var (
		ringMu       sync.Mutex
		latestSeq    uint64
		lastState    gameInputGamepadState
		latchedBtns  uint32 // Boutons maintenus au moins jusqu'à la prochaine lecture du jeu (anti-perte d'appui court)
		readingCbs   []struct{ tok uint64; ctx, cb uintptr }
	)
	autoMode := os.Getenv("ASTRAL_AUTO_INPUT")
	slotPtr := func(seq uint64) uintptr {
		return ringMem + uintptr((seq-1)%ringCap)*64
	}
	pushReadingLocked := func(st gameInputGamepadState) uintptr {
		latestSeq++
		p := slotPtr(latestSeq)
		*(*uintptr)(osPtr(p + 0x00)) = readingVtable
		*(*uint64)(osPtr(p + 0x08)) = latestSeq
		*(*uint64)(osPtr(p + 0x10)) = nowUS()
		*(*gameInputGamepadState)(osPtr(p + 0x18)) = st
		lastState = st
		return p
	}
	sampleIfChanged := func(consumeLatch bool) (uintptr, bool) {
		st := pollLiveGamepad()
		if autoMode != "" {
			elapsed := time.Since(startTime)
			if elapsed > 45*time.Second && elapsed < 120*time.Second && (elapsed.Milliseconds()%2000) < 250 {
				if autoMode == "y" {
					st.Buttons |= giBtnY
				} else {
					st.Buttons |= giBtnA
				}
			}
		}
		ringMu.Lock()
		latchedBtns |= st.Buttons
		eff := st
		eff.Buttons |= latchedBtns
		if consumeLatch {
			latchedBtns = st.Buttons
		}
		if latestSeq == 0 || eff != lastState {
			p := pushReadingLocked(eff)
			cbs := append([]struct{ tok uint64; ctx, cb uintptr }(nil), readingCbs...)
			ringMu.Unlock()
			for _, c := range cbs {
				if c.cb >= 0x10000 && c.cb < 0x800000000000 {
					_, _, _ = syscall.SyscallN(c.cb, uintptr(c.tok), c.ctx, p)
				}
			}
			return p, true
		}
		p := slotPtr(latestSeq)
		ringMu.Unlock()
		return p, false
	}
	// Initialise le tout premier reading (état neutre) puis poll toutes les 8 ms en arrière-plan
	_, _ = sampleIfChanged(false)
	go func() {
		tk := time.NewTicker(8 * time.Millisecond)
		defer tk.Stop()
		for range tk.C {
			_, _ = sampleIfChanged(false)
		}
	}()

	cbGetTimestamp := syscall.NewCallback(func(this uintptr) uintptr {
		logGI(6, "GetCurrentTimestamp")
		return uintptr(nowUS())
	})
	cbGetCurrentReading := syscall.NewCallback(func(this, inputKind, device, outReading uintptr) uintptr {
		kind := uint32(inputKind)
		logGI(7, "GetCurrentReading", "kind", fmt.Sprintf("0x%X", kind), "dev", fmt.Sprintf("0x%X", device))
		if device == kbdDeviceObj || (kind&giKindKeyboard != 0 && kind&giKindGamepad == 0) {
			if outReading >= 0x10000 {
				kbdSeq.Add(1)
				*(*uintptr)(osPtr(outReading)) = kbdReadingObj
				return hresultOK
			}
		}
		if kind == 0 || kind&(giKindGamepad|0x01000000) != 0 {
			if outReading >= 0x10000 {
				p, _ := sampleIfChanged(true)
				*(*uintptr)(osPtr(outReading)) = p
				return hresultOK
			}
		}
		if outReading >= 0x10000 {
			*(*uintptr)(osPtr(outReading)) = 0
		}
		return giReadingNotFound
	})
	cbGetNextReading := syscall.NewCallback(func(this, refReading, inputKind, device, outReading uintptr) uintptr {
		kind := uint32(inputKind)
		logGI(8, "GetNextReading", "kind", fmt.Sprintf("0x%X", kind))
		if (kind != 0 && kind&giKindGamepad == 0) || refReading < 0x10000 || outReading < 0x10000 {
			return giReadingNotFound
		}
		_, _ = sampleIfChanged(true)
		refSeq := *(*uint64)(osPtr(refReading + 0x08))
		ringMu.Lock()
		curLatest := latestSeq
		if refSeq >= curLatest || curLatest == 0 {
			ringMu.Unlock()
			return giReadingNotFound
		}
		nextSeq := refSeq + 1
		if curLatest-refSeq >= ringCap {
			nextSeq = curLatest - ringCap + 1
		}
		p := slotPtr(nextSeq)
		ringMu.Unlock()
		*(*uintptr)(osPtr(outReading)) = p
		return hresultOK
	})
	cbGetPrevReading := syscall.NewCallback(func(this, refReading, inputKind, device, outReading uintptr) uintptr {
		kind := uint32(inputKind)
		logGI(9, "GetPreviousReading", "kind", fmt.Sprintf("0x%X", kind))
		if (kind != 0 && kind&giKindGamepad == 0) || refReading < 0x10000 || outReading < 0x10000 {
			return giReadingNotFound
		}
		refSeq := *(*uint64)(osPtr(refReading + 0x08))
		ringMu.Lock()
		curLatest := latestSeq
		if refSeq <= 1 || curLatest-refSeq+1 >= ringCap {
			ringMu.Unlock()
			return giReadingNotFound
		}
		p := slotPtr(refSeq - 1)
		ringMu.Unlock()
		*(*uintptr)(osPtr(outReading)) = p
		return hresultOK
	})
	cbRegReading := syscall.NewCallback(func(this, dev, kind, thresh, ctx, cb, outToken uintptr) uintptr {
		tok := nextToken.Add(1)
		if outToken >= 0x10000 {
			*(*uint64)(osPtr(outToken)) = tok
		}
		if (uint32(kind) == 0 || uint32(kind)&giKindGamepad != 0) && cb >= 0x10000 && cb < 0x800000000000 {
			ringMu.Lock()
			readingCbs = append(readingCbs, struct{ tok uint64; ctx, cb uintptr }{tok: tok, ctx: ctx, cb: cb})
			ringMu.Unlock()
		}
		return hresultOK
	})
	cbRegDevice := syscall.NewCallback(func(this, dev, kind, status, enumKind, ctx, cb, outToken uintptr) uintptr {
		tok := nextToken.Add(1)
		if outToken >= 0x10000 {
			*(*uint64)(osPtr(outToken)) = tok
		}
		slog.Info("IGameInput!RegisterDeviceCallback",
			"kind", fmt.Sprintf("0x%X", uint32(kind)),
			"status", fmt.Sprintf("0x%X", uint32(status)),
			"enumKind", uint32(enumKind),
		)
		if uint32(enumKind) != 0 && cb >= 0x10000 && cb < 0x800000000000 {
			var devs []uintptr
			if uint32(kind) == 0 || uint32(kind)&giKindGamepad != 0 {
				devs = append(devs, deviceObj)
			}
			if uint32(kind) == 0 || uint32(kind)&giKindKeyboard != 0 {
				devs = append(devs, kbdDeviceObj)
			}
			for _, d := range devs {
				d := d
				if uint32(enumKind) == 1 { // GameInputAsyncEnumeration -> après retour de RegisterDeviceCallback !
					go func() {
						time.Sleep(10 * time.Millisecond)
						_, _, _ = syscall.SyscallN(cb, uintptr(tok), ctx, d, uintptr(nowUS()), uintptr(giDeviceConnected), 0)
					}()
				} else { // GameInputBlockingEnumeration (2) -> synchrone
					_, _, _ = syscall.SyscallN(cb, uintptr(tok), ctx, d, uintptr(nowUS()), uintptr(giDeviceConnected), 0)
				}
			}
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

	cbFindById := syscall.NewCallback(func(this, idPtr, outDev uintptr) uintptr {
		if outDev >= 0x10000 {
			*(*uintptr)(osPtr(outDev)) = deviceObj
			return hresultOK
		}
		return hresultInvalidArg
	})
	cbDevNotFound := syscall.NewCallback(func(this, val, outDev uintptr) uintptr {
		if outDev >= 0x10000 {
			*(*uintptr)(osPtr(outDev)) = 0
		}
		return 0x838A0002 // GAMEINPUT_E_DEVICE_NOT_FOUND
	})

	gameInputObj := NewCOMObject("IGameInput", 22, map[int]uintptr{
		3:  cbGetTimestamp,      // 0x18 GetCurrentTimestamp
		4:  cbGetCurrentReading, // 0x20 GetCurrentReading (XInput/HID + Clavier live, snapshots ring-buffer)
		5:  cbGetNextReading,    // 0x28 GetNextReading
		6:  cbGetPrevReading,    // 0x30 GetPreviousReading
		7:  cbNotFound,          // 0x38 GetTemporalReading
		8:  cbRegReading,        // 0x40 RegisterReadingCallback (7 args)
		9:  cbRegDevice,         // 0x48 RegisterDeviceCallback (8 args)
		10: cbReg5,              // 0x50 RegisterGuideButtonCallback
		11: cbReg5,              // 0x58 RegisterKeyboardLayoutCallback
		12: cbOK,                // 0x60 StopCallback
		13: cbTrue,              // 0x68 UnregisterCallback
		14: cbOK,                // 0x70 CreateDispatcher
		15: cbOK,                // 0x78 CreateAggregateDevice
		16: cbFindById,          // 0x80 FindDeviceFromId
		17: cbDevNotFound,       // 0x88 FindDeviceFromObject
		18: cbDevNotFound,       // 0x90 FindDeviceFromPlatformHandle
		19: cbDevNotFound,       // 0x98 FindDeviceFromPlatformString
		20: cbOK,                // 0xA0 EnableOemDeviceSupport
		21: cbOK,                // 0xA8 SetFocusPolicy
	})
	_ = cbRegSysBtn

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
