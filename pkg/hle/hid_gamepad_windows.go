//go:build windows

package hle

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	modSetupAPI                 = syscall.NewLazyDLL("setupapi.dll")
	procSetupDiGetClassDevsW    = modSetupAPI.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceIfaces = modSetupAPI.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetIfaceDetailW  = modSetupAPI.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDevInfo   = modSetupAPI.NewProc("SetupDiDestroyDeviceInfoList")

	modHID                   = syscall.NewLazyDLL("hid.dll")
	procHidDGetHidGuid       = modHID.NewProc("HidD_GetHidGuid")
	procHidDGetAttributes    = modHID.NewProc("HidD_GetAttributes")
	procHidDGetPreparsedData = modHID.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsed    = modHID.NewProc("HidD_FreePreparsedData")
	procHidPGetCaps          = modHID.NewProc("HidP_GetCaps")
	procHidPGetValueCaps     = modHID.NewProc("HidP_GetValueCaps")
	procHidPGetUsages        = modHID.NewProc("HidP_GetUsages")
	procHidPGetUsageValue    = modHID.NewProc("HidP_GetUsageValue")

	k32ReadFile = k32.NewProc("ReadFile")
)

type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGuid [16]byte
	flags              uint32
	reserved           uintptr
}

type hidAttributes struct {
	size          uint32
	vendorID      uint16
	productID     uint16
	versionNumber uint16
	_pad          uint16
}

type hidPCaps struct {
	usage                     uint16
	usagePage                 uint16
	inputReportByteLength     uint16
	outputReportByteLength    uint16
	featureReportByteLength   uint16
	reserved                  [17]uint16
	numberLinkCollectionNodes uint16
	numberInputButtonCaps     uint16
	numberInputValueCaps      uint16
	numberInputDataIndices    uint16
	numberOutputButtonCaps    uint16
	numberOutputValueCaps     uint16
	numberOutputDataIndices   uint16
	numberFeatureButtonCaps   uint16
	numberFeatureValueCaps    uint16
	numberFeatureDataIndices  uint16
}

// hidPValueCaps correspond à HIDP_VALUE_CAPS (72 octets sur Windows x64).
type hidPValueCaps struct {
	usagePage         uint16
	reportID          uint8
	isAlias           uint8
	bitField          uint16
	linkCollection    uint16
	linkUsage         uint16
	linkUsagePage     uint16
	isRange           uint8
	isStringRange     uint8
	isDesignatorRange uint8
	isAbsolute        uint8
	hasNull           uint8
	reserved          uint8
	bitSize           uint16
	reportCount       uint16
	reserved2         [5]uint16
	unitsExp          uint32
	units             uint32
	logicalMin        int32
	logicalMax        int32
	physicalMin       int32
	physicalMax       int32
	usageMin          uint16
	usageMax          uint16
	stringMin         uint16
	stringMax         uint16
	designatorMin     uint16
	designatorMax     uint16
	dataIndexMin      uint16
	dataIndexMax      uint16
}

type hidAxisRange struct {
	min     int32
	max     int32
	present bool
}

type hidAxisMap struct {
	x, y, z, rx, ry, rz hidAxisRange
	hat                 hidAxisRange
	brake, accel        hidAxisRange // Simulation Controls (0x02): 0xC5 = L2 (Brake), 0xC4 = R2 (Accelerator) — utilisé par Stadia
}

type hidGamepadManager struct {
	once    sync.Once
	mu      sync.Mutex
	active  map[string]bool
	states  map[string]gameInputGamepadState
	running atomic.Bool
}

var hidManager hidGamepadManager

func (m *hidGamepadManager) start() {
	m.once.Do(func() {
		m.active = make(map[string]bool)
		m.states = make(map[string]gameInputGamepadState)
		m.running.Store(true)
		m.scanOnce()
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				m.scanOnce()
			}
		}()
	})
}

func (m *hidGamepadManager) mergedState() (gameInputGamepadState, bool) {
	m.start()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.states) == 0 {
		return gameInputGamepadState{}, false
	}
	var out gameInputGamepadState
	for _, s := range m.states {
		out.Buttons |= s.Buttons
		if s.LeftTrigger > out.LeftTrigger {
			out.LeftTrigger = s.LeftTrigger
		}
		if s.RightTrigger > out.RightTrigger {
			out.RightTrigger = s.RightTrigger
		}
		if absF32(s.LeftThumbstickX) > absF32(out.LeftThumbstickX) {
			out.LeftThumbstickX = s.LeftThumbstickX
		}
		if absF32(s.LeftThumbstickY) > absF32(out.LeftThumbstickY) {
			out.LeftThumbstickY = s.LeftThumbstickY
		}
		if absF32(s.RightThumbstickX) > absF32(out.RightThumbstickX) {
			out.RightThumbstickX = s.RightThumbstickX
		}
		if absF32(s.RightThumbstickY) > absF32(out.RightThumbstickY) {
			out.RightThumbstickY = s.RightThumbstickY
		}
	}
	return out, true
}

func absF32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

func (m *hidGamepadManager) scanOnce() {
	if procHidDGetHidGuid.Find() != nil || procSetupDiGetClassDevsW.Find() != nil {
		return
	}
	var hidGUID [16]byte
	_, _, _ = procHidDGetHidGuid.Call(uintptr(unsafe.Pointer(&hidGUID[0])))

	const digcfPresent = 0x02
	const digcfDeviceInterface = 0x10
	devInfo, _, _ := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&hidGUID[0])),
		0, 0,
		digcfPresent|digcfDeviceInterface,
	)
	if devInfo == 0 || devInfo == ^uintptr(0) {
		return
	}
	defer procSetupDiDestroyDevInfo.Call(devInfo)

	for idx := uintptr(0); idx < 256; idx++ {
		var ifData spDeviceInterfaceData
		ifData.cbSize = uint32(unsafe.Sizeof(ifData))
		ok, _, _ := procSetupDiEnumDeviceIfaces.Call(
			devInfo, 0,
			uintptr(unsafe.Pointer(&hidGUID[0])),
			idx,
			uintptr(unsafe.Pointer(&ifData)),
		)
		if ok == 0 {
			break
		}
		var reqSize uint32
		_, _, _ = procSetupDiGetIfaceDetailW.Call(devInfo, uintptr(unsafe.Pointer(&ifData)), 0, 0, uintptr(unsafe.Pointer(&reqSize)), 0)
		if reqSize < 8 || reqSize > 4096 {
			continue
		}
		detailBuf := make([]byte, reqSize)
		// SP_DEVICE_INTERFACE_DETAIL_DATA_W : cbSize = 8 sur amd64, DevicePath UTF-16 à +4
		*(*uint32)(unsafe.Pointer(&detailBuf[0])) = 8
		ok, _, _ = procSetupDiGetIfaceDetailW.Call(
			devInfo,
			uintptr(unsafe.Pointer(&ifData)),
			uintptr(unsafe.Pointer(&detailBuf[0])),
			uintptr(reqSize),
			0, 0,
		)
		if ok == 0 {
			continue
		}
		pathUTF16 := unsafe.Slice((*uint16)(unsafe.Pointer(&detailBuf[4])), (reqSize-4)/2)
		devPath := syscall.UTF16ToString(pathUTF16)
		if devPath == "" {
			continue
		}
		m.mu.Lock()
		already := m.active[devPath]
		m.mu.Unlock()
		if already {
			continue
		}

		m.tryOpenHID(devPath)
	}
}

func (m *hidGamepadManager) tryOpenHID(devPath string) {
	pathPtr, err := syscall.UTF16PtrFromString(devPath)
	if err != nil {
		return
	}
	const (
		genericRead    = 0x80000000
		fileShareRead  = 0x00000001
		fileShareWrite = 0x00000002
		openExisting   = 3
	)
	h, _, _ := k32CreateFileW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		genericRead,
		fileShareRead|fileShareWrite,
		0,
		openExisting,
		0,
		0,
	)
	if h == 0 || h == ^uintptr(0) {
		return
	}

	var preparsed uintptr
	if ok, _, _ := procHidDGetPreparsedData.Call(h, uintptr(unsafe.Pointer(&preparsed))); ok == 0 || preparsed == 0 {
		_, _, _ = k32CloseHandle.Call(h)
		return
	}
	var caps hidPCaps
	if status, _, _ := procHidPGetCaps.Call(preparsed, uintptr(unsafe.Pointer(&caps))); status != 0x00110000 /* HIDP_STATUS_SUCCESS */ {
		_, _, _ = procHidDFreePreparsed.Call(preparsed)
		_, _, _ = k32CloseHandle.Call(h)
		return
	}
	// Uniquement Generic Desktop (0x01) -> Joystick (0x04) ou Gamepad (0x05)
	if caps.usagePage != 0x01 || (caps.usage != 0x04 && caps.usage != 0x05) || caps.inputReportByteLength == 0 {
		_, _, _ = procHidDFreePreparsed.Call(preparsed)
		_, _, _ = k32CloseHandle.Call(h)
		return
	}

	var attrs hidAttributes
	attrs.size = uint32(unsafe.Sizeof(attrs))
	_, _, _ = procHidDGetAttributes.Call(h, uintptr(unsafe.Pointer(&attrs)))
	// Laisse XInput gérer nativement les manettes Microsoft Xbox (0x045E)
	if attrs.vendorID == 0x045E {
		_, _, _ = procHidDFreePreparsed.Call(preparsed)
		_, _, _ = k32CloseHandle.Call(h)
		return
	}

	axes := parseHIDAxes(preparsed, caps.numberInputValueCaps)

	m.mu.Lock()
	m.active[devPath] = true
	m.states[devPath] = gameInputGamepadState{}
	m.mu.Unlock()

	kindName := "HID générique"
	switch attrs.vendorID {
	case 0x18D1:
		kindName = "Google Stadia"
	case 0x054C:
		kindName = "PlayStation DualSense/DualShock"
	case 0x057E:
		kindName = "Nintendo Switch"
	}
	slog.Info("GameInput manette HID universelle connectée",
		"type", kindName,
		"vid", fmt.Sprintf("0x%04X", attrs.vendorID),
		"pid", fmt.Sprintf("0x%04X", attrs.productID),
		"reportLen", caps.inputReportByteLength,
	)

	go m.readLoop(devPath, h, preparsed, caps.inputReportByteLength, attrs.vendorID, attrs.productID, axes)
}

func parseHIDAxes(preparsed uintptr, count uint16) hidAxisMap {
	var axes hidAxisMap
	if count == 0 || count > 64 {
		return axes
	}
	vCaps := make([]hidPValueCaps, count)
	n := count
	st, _, _ := procHidPGetValueCaps.Call(0 /* HidP_Input */, uintptr(unsafe.Pointer(&vCaps[0])), uintptr(unsafe.Pointer(&n)), preparsed)
	if st != 0x00110000 {
		return axes
	}
	for i := uint16(0); i < n; i++ {
		vc := vCaps[i]
		r := hidAxisRange{min: vc.logicalMin, max: vc.logicalMax, present: true}
		if r.max <= r.min {
			if vc.bitSize > 0 && vc.bitSize <= 16 {
				r.min = 0
				r.max = (1 << vc.bitSize) - 1
			} else {
				r.min = 0
				r.max = 255
			}
		}
		uStart, uEnd := vc.usageMin, vc.usageMin
		if vc.isRange != 0 && vc.usageMax >= vc.usageMin {
			uEnd = vc.usageMax
		}
		for u := uStart; u <= uEnd; u++ {
			switch vc.usagePage {
			case 0x01: // Generic Desktop
				switch u {
				case 0x30:
					axes.x = r
				case 0x31:
					axes.y = r
				case 0x32:
					axes.z = r
				case 0x33:
					axes.rx = r
				case 0x34:
					axes.ry = r
				case 0x35:
					axes.rz = r
				case 0x39:
					axes.hat = r
				}
			case 0x02: // Simulation Controls (gâchettes analogiques Stadia/Xbox BT)
				switch u {
				case 0xC5: // Brake -> L2
					axes.brake = r
				case 0xC4: // Accelerator -> R2
					axes.accel = r
				}
			}
		}
	}
	return axes
}

func normHIDAxis(raw uint32, r hidAxisRange, invert bool) float32 {
	if !r.present || r.max <= r.min {
		return 0
	}
	var v float32
	if r.min < 0 {
		v = float32(int32(raw))
	} else {
		v = float32(raw)
	}
	mid := float32(r.min+r.max) * 0.5
	half := float32(r.max-r.min) * 0.5
	if half <= 0 {
		return 0
	}
	f := (v - mid) / half
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

func normHIDTrigger(raw uint32, r hidAxisRange) float32 {
	if !r.present || r.max <= r.min {
		return 0
	}
	f := float32(int64(raw)-int64(r.min)) / float32(int64(r.max)-int64(r.min))
	if f < 0.03 {
		return 0
	}
	if f > 1.0 {
		return 1.0
	}
	return f
}

func (m *hidGamepadManager) readLoop(devPath string, h, preparsed uintptr, reportLen, vid, pid uint16, axes hidAxisMap) {
	defer func() {
		_, _, _ = procHidDFreePreparsed.Call(preparsed)
		_, _, _ = k32CloseHandle.Call(h)
		m.mu.Lock()
		delete(m.active, devPath)
		delete(m.states, devPath)
		m.mu.Unlock()
	}()

	buf := make([]byte, reportLen)
	usages := make([]uint16, 32)
	isStadia := vid == 0x18D1
	isSony := vid == 0x054C

	for {
		var nRead uint32
		ok, _, _ := k32ReadFile.Call(
			h,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(reportLen),
			uintptr(unsafe.Pointer(&nRead)),
			0,
		)
		if ok == 0 || nRead == 0 {
			return
		}

		var st gameInputGamepadState

		// 1. Boutons HID (UsagePage 0x09 = Button, usages 1..32)
		usageLen := uint32(len(usages))
		status, _, _ := procHidPGetUsages.Call(
			0, // HidP_Input
			0x09,
			0,
			uintptr(unsafe.Pointer(&usages[0])),
			uintptr(unsafe.Pointer(&usageLen)),
			preparsed,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(nRead),
		)
		if status == 0x00110000 {
			for i := uint32(0); i < usageLen; i++ {
				u := usages[i]
				if isStadia {
					// Google Stadia Controller (VID 0x18D1, PID 0x9400) USB & Bluetooth HID :
					// 1=A, 2=B, 3=X, 4=Y, 5=L1, 6=R1, 7=L3, 8=R3, 9=Options(View), 10=Menu, 12=R2, 13=L2
					switch u {
					case 1:
						st.Buttons |= giBtnA
					case 2:
						st.Buttons |= giBtnB
					case 3:
						st.Buttons |= giBtnX
					case 4:
						st.Buttons |= giBtnY
					case 5:
						st.Buttons |= giBtnLeftShoulder
					case 6:
						st.Buttons |= giBtnRightShoulder
					case 7:
						st.Buttons |= giBtnLeftThumb
					case 8:
						st.Buttons |= giBtnRightThumb
					case 9:
						st.Buttons |= giBtnView
					case 10:
						st.Buttons |= giBtnMenu
					case 12:
						st.RightTrigger = 1.0
					case 13:
						st.LeftTrigger = 1.0
					}
				} else if isSony {
					// Sony DualSense / DualShock 4 HID : 1=Carré(X), 2=Croix(A), 3=Rond(B), 4=Triangle(Y)
					switch u {
					case 1:
						st.Buttons |= giBtnX
					case 2:
						st.Buttons |= giBtnA
					case 3:
						st.Buttons |= giBtnB
					case 4:
						st.Buttons |= giBtnY
					case 5:
						st.Buttons |= giBtnLeftShoulder
					case 6:
						st.Buttons |= giBtnRightShoulder
					case 7:
						st.LeftTrigger = 1.0
					case 8:
						st.RightTrigger = 1.0
					case 9, 14:
						st.Buttons |= giBtnView
					case 10:
						st.Buttons |= giBtnMenu
					case 11:
						st.Buttons |= giBtnLeftThumb
					case 12:
						st.Buttons |= giBtnRightThumb
					}
				} else {
					// Manette HID générique standard (layout Xbox A/B/X/Y : 1=A, 2=B, 3=X, 4=Y, 5=LB, 6=RB, 7=View, 8=Menu, 9=L3, 10=R3)
					switch u {
					case 1:
						st.Buttons |= giBtnA
					case 2:
						st.Buttons |= giBtnB
					case 3:
						st.Buttons |= giBtnX
					case 4:
						st.Buttons |= giBtnY
					case 5:
						st.Buttons |= giBtnLeftShoulder
					case 6:
						st.Buttons |= giBtnRightShoulder
					case 7:
						st.Buttons |= giBtnView
					case 8:
						st.Buttons |= giBtnMenu
					case 9:
						st.Buttons |= giBtnLeftThumb
					case 10:
						st.Buttons |= giBtnRightThumb
					}
				}
			}
		}

		// 2. Axes analogiques et Croix directionnelle (Hat Switch)
		readVal := func(page, usage uint16) (uint32, bool) {
			var val uint32
			r, _, _ := procHidPGetUsageValue.Call(
				0, // HidP_Input
				uintptr(page),
				0,
				uintptr(usage),
				uintptr(unsafe.Pointer(&val)),
				preparsed,
				uintptr(unsafe.Pointer(&buf[0])),
				uintptr(nRead),
			)
			return val, r == 0x00110000
		}

		if axes.x.present {
			if v, ok := readVal(0x01, 0x30); ok {
				st.LeftThumbstickX = normHIDAxis(v, axes.x, false)
			}
		}
		if axes.y.present {
			if v, ok := readVal(0x01, 0x31); ok {
				st.LeftThumbstickY = normHIDAxis(v, axes.y, true)
			}
		}
		// Stick droit : Z/Rz (Stadia, DualSense, la plupart des HID) ou Rx/Ry
		if axes.z.present && axes.rz.present {
			if v, ok := readVal(0x01, 0x32); ok {
				st.RightThumbstickX = normHIDAxis(v, axes.z, false)
			}
			if v, ok := readVal(0x01, 0x35); ok {
				st.RightThumbstickY = normHIDAxis(v, axes.rz, true)
			}
			// Gâchettes sur Rx/Ry (DualSense / génériques) si présentes
			if axes.rx.present && !axes.brake.present {
				if v, ok := readVal(0x01, 0x33); ok {
					if lt := normHIDTrigger(v, axes.rx); lt > st.LeftTrigger {
						st.LeftTrigger = lt
					}
				}
			}
			if axes.ry.present && !axes.accel.present {
				if v, ok := readVal(0x01, 0x34); ok {
					if rt := normHIDTrigger(v, axes.ry); rt > st.RightTrigger {
						st.RightTrigger = rt
					}
				}
			}
		} else if axes.rx.present && axes.ry.present {
			if v, ok := readVal(0x01, 0x33); ok {
				st.RightThumbstickX = normHIDAxis(v, axes.rx, false)
			}
			if v, ok := readVal(0x01, 0x34); ok {
				st.RightThumbstickY = normHIDAxis(v, axes.ry, true)
			}
		}

		// Gâchettes Simulation Controls (Stadia : Brake 0xC5 = L2, Accelerator 0xC4 = R2)
		if axes.brake.present {
			if v, ok := readVal(0x02, 0xC5); ok {
				if lt := normHIDTrigger(v, axes.brake); lt > st.LeftTrigger {
					st.LeftTrigger = lt
				}
			}
		}
		if axes.accel.present {
			if v, ok := readVal(0x02, 0xC4); ok {
				if rt := normHIDTrigger(v, axes.accel); rt > st.RightTrigger {
					st.RightTrigger = rt
				}
			}
		}

		// Croix directionnelle (Hat Switch 0x39)
		if axes.hat.present {
			if v, ok := readVal(0x01, 0x39); ok {
				dir := int32(v) - axes.hat.min
				// 0=N, 1=NE, 2=E, 3=SE, 4=S, 5=SW, 6=W, 7=NW (hors [0..7] = neutre)
				if dir >= 0 && dir <= 7 {
					if dir == 7 || dir == 0 || dir == 1 {
						st.Buttons |= giBtnDPadUp
					}
					if dir == 1 || dir == 2 || dir == 3 {
						st.Buttons |= giBtnDPadRight
					}
					if dir == 3 || dir == 4 || dir == 5 {
						st.Buttons |= giBtnDPadDown
					}
					if dir == 5 || dir == 6 || dir == 7 {
						st.Buttons |= giBtnDPadLeft
					}
				}
			}
		}

		m.mu.Lock()
		m.states[devPath] = st
		m.mu.Unlock()
	}
}
