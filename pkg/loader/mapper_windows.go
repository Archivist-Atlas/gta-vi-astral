//go:build windows

package loader

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/hle"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/pe"
)

// MappedModule représente une image PE32+ mappée dans l'espace d'adressage du processus.
type MappedModule struct {
	Name string
	Path string
	Base uintptr
	Size uint32
	Info *pe.ImageInfo
}

// LiveSession regroupe l'ensemble des modules mappés et la table de résolution active.
type LiveSession struct {
	Main          *MappedModule
	Modules       []*MappedModule
	modulesByName map[string]*MappedModule
	hostModules   map[string]uintptr
	BoundHLE      int
	BoundHost     int
	BoundTitleDLL int
	BoundStubs    int
	TLSSlot       uint32
	TLSPatched    int
	DataDir       string
}

var (
	vehOnce    sync.Once
	vehMu      sync.RWMutex
	vehSession *LiveSession
)

// MapOptions configure le mappage natif Windows d'un exécutable console.
type MapOptions struct {
	ExePath       string
	CacheDir      string
	DataDir       string
	Unimplemented core.UnimplementedPolicy
	LiveHLE       *hle.LiveTable
	HostPolicy    hle.HostPolicy
}

// MapAndBind prépare l'exécutable en DLL passive, le mappe via Windows, mappe les DLL
// du titre importées, redirige le TLS statique et lie tous les slots IAT.
func MapAndBind(opts MapOptions) (*LiveSession, error) {
	if err := core.EnsureOutsideRepo(opts.CacheDir); err != nil {
		return nil, err
	}
	if opts.DataDir != "" {
		if err := core.EnsureOutsideRepo(opts.DataDir); err != nil {
			return nil, err
		}
		SetCrashDataDir(opts.DataDir)
	}

	raw, err := os.ReadFile(opts.ExePath)
	if err != nil {
		return nil, err
	}
	prepared, err := PreparePassiveDLL(raw)
	if err != nil {
		return nil, fmt.Errorf("préparation passive-DLL : %w", err)
	}

	imgDir := filepath.Join(opts.CacheDir, "images")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return nil, err
	}
	dllPath := filepath.Join(imgDir, prepared.FileName())
	if _, statErr := os.Stat(dllPath); statErr != nil {
		tmp := dllPath + ".tmp"
		if err := os.WriteFile(tmp, prepared.Bytes, 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, dllPath); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
	}

	base, err := loadLibraryExW(dllPath, loadWithAlteredSearchPath)
	if err != nil {
		return nil, fmt.Errorf("mappage Windows de %s : %w", filepath.Base(opts.ExePath), err)
	}

	mainMod := &MappedModule{
		Name: filepath.Base(opts.ExePath),
		Path: opts.ExePath,
		Base: base,
		Size: prepared.Info.SizeOfImage,
		Info: prepared.Info,
	}
	hle.SetMainImageBase(base)
	sess := &LiveSession{
		Main:          mainMod,
		Modules:       []*MappedModule{mainMod},
		modulesByName: map[string]*MappedModule{strings.ToLower(mainMod.Name): mainMod},
		hostModules:   make(map[string]uintptr),
		DataDir:       opts.DataDir,
	}

	// Redirection TLS statique pour l'exécutable principal (slot != 0 -> slot 0 logique)
	patched, slot, err := RedirectStaticTLS(base, prepared.Info)
	if err != nil {
		return nil, fmt.Errorf("redirection TLS statique : %w", err)
	}
	sess.TLSPatched = patched
	sess.TLSSlot = slot

	// Prépare le bloc de thunks pour les imports non encore implémentés en live
	thunks, err := NewThunkBlock(prepared.Info.TotalImports() + 128)
	if err != nil {
		return nil, err
	}

	titleDir := filepath.Dir(opts.ExePath)
	if err := sess.bindModule(mainMod, titleDir, imgDir, opts, thunks); err != nil {
		return nil, err
	}

	if err := thunks.Seal(); err != nil {
		return nil, err
	}

	installVEH(sess)
	return sess, nil
}

func (s *LiveSession) bindModule(mod *MappedModule, titleDir, imgDir string, opts MapOptions, thunks *ThunkBlock) error {
	for _, impMod := range mod.Info.Imports {
		for _, entry := range impMod.Entries {
			sym := entry.DisplayName()
			addr, kind, err := s.resolveAddress(impMod.DLL, entry, titleDir, imgDir, opts, thunks)
			if err != nil {
				return fmt.Errorf("résolution %s!%s : %w", impMod.DLL, sym, err)
			}
			slotAddr := mod.Base + uintptr(entry.IATSlotRVA)
			oldProt, err := virtualProtect(slotAddr, 8, pageReadWrite)
			if err != nil {
				return fmt.Errorf("VirtualProtect(IAT %s!%s) : %w", impMod.DLL, sym, err)
			}
			*(*uintptr)(ptrFromAddr(slotAddr)) = addr
			_, _ = virtualProtect(slotAddr, 8, oldProt)

			switch kind {
			case ResolveHLE:
				s.BoundHLE++
			case ResolveHost:
				s.BoundHost++
			case ResolveTitleDLL:
				s.BoundTitleDLL++
			default:
				s.BoundStubs++
			}
		}
	}
	return nil
}

func (s *LiveSession) resolveAddress(dll string, entry pe.ImportEntry, titleDir, imgDir string, opts MapOptions, thunks *ThunkBlock) (uintptr, ResolutionKind, error) {
	sym := entry.DisplayName()

	// 1. Live HLE override (toujours prioritaire)
	if opts.LiveHLE != nil {
		if fn, ok := opts.LiveHLE.Lookup(dll, sym); ok && fn != 0 {
			return fn, ResolveHLE, nil
		}
	}

	// 2. Host System32 (protégé contre le DLL planting via LOAD_LIBRARY_SEARCH_SYSTEM32)
	if opts.HostPolicy.Allows(dll) {
		hMod := s.loadHostDLL(dll)
		if hMod != 0 {
			var proc uintptr
			if entry.ByOrdinal {
				proc = getProcAddressOrdinal(hMod, entry.Ordinal)
			} else {
				proc = getProcAddressName(hMod, entry.Name)
			}
			if proc != 0 {
				return proc, ResolveHost, nil
			}
		}
	}

	// 3. DLL fournie par le titre (ex: Bink2Scarlett.dll, XCurl.dll, msvcp140.dll)
	titlePath := filepath.Join(titleDir, dll)
	if _, err := os.Stat(titlePath); err == nil {
		tMod, err := s.ensureTitleDLL(dll, titlePath, imgDir, titleDir, opts, thunks)
		if err == nil && tMod != nil {
			if addr := lookupExport(tMod, entry); addr != 0 {
				return addr, ResolveTitleDLL, nil
			}
		}
	}

	// 4. Stub x86-64 généré
	stub, err := thunks.Emit(dll, sym, opts.Unimplemented)
	if err != nil {
		return 0, ResolveUnimplemented, err
	}
	return stub, ResolveUnimplemented, nil
}

func (s *LiveSession) loadHostDLL(dll string) uintptr {
	key := strings.ToLower(dll)
	if h, ok := s.hostModules[key]; ok {
		return h
	}
	h, err := loadLibraryExW(dll, loadLibrarySearchSystem32)
	if err != nil {
		s.hostModules[key] = 0
		return 0
	}
	s.hostModules[key] = h
	return h
}

func (s *LiveSession) ensureTitleDLL(dll, fullPath, imgDir, titleDir string, opts MapOptions, thunks *ThunkBlock) (*MappedModule, error) {
	key := strings.ToLower(dll)
	if m, ok := s.modulesByName[key]; ok {
		return m, nil
	}
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, err
	}
	prep, err := PreparePassiveDLL(raw)
	if err != nil {
		return nil, err
	}
	outPath := filepath.Join(imgDir, prep.FileName())
	if _, err := os.Stat(outPath); err != nil {
		tmp := outPath + ".tmp"
		if err := os.WriteFile(tmp, prep.Bytes, 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, outPath); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
	}
	base, err := loadLibraryExW(outPath, loadWithAlteredSearchPath)
	if err != nil {
		return nil, err
	}
	mod := &MappedModule{
		Name: dll,
		Path: fullPath,
		Base: base,
		Size: prep.Info.SizeOfImage,
		Info: prep.Info,
	}
	s.modulesByName[key] = mod
	s.Modules = append(s.Modules, mod)
	// Lie récursivement les imports de la DLL du titre
	_ = s.bindModule(mod, titleDir, imgDir, opts, thunks)
	return mod, nil
}

func lookupExport(mod *MappedModule, entry pe.ImportEntry) uintptr {
	for _, exp := range mod.Info.Exports {
		if entry.ByOrdinal {
			if exp.Ordinal == entry.Ordinal && exp.RVA != 0 {
				return mod.Base + uintptr(exp.RVA)
			}
		} else if strings.EqualFold(exp.Name, entry.Name) && exp.RVA != 0 {
			return mod.Base + uintptr(exp.RVA)
		}
	}
	return 0
}

// RunEntryPoint exécute les callbacks TLS (DLL_PROCESS_ATTACH) puis le point d'entrée
// du titre sur un thread OS dédié muni d'une pile de 8 Mio (approche Leonida).
func (s *LiveSession) RunEntryPoint() (uint32, error) {
	if s.Main == nil || s.Main.Info.EntryRVA == 0 {
		return 0, fmt.Errorf("point d'entrée invalide")
	}

	// 0. Exécute DllMain(DLL_PROCESS_ATTACH) des DLL du titre chargées en mode passif
	for _, mod := range s.Modules {
		if mod == s.Main || mod.Info == nil || mod.Info.EntryRVA == 0 {
			continue
		}
		dllMain := mod.Base + uintptr(mod.Info.EntryRVA)
		slog.Info("exécution DllMain titre", "module", mod.Name, "rva", fmt.Sprintf("0x%X", mod.Info.EntryRVA))
		_, _, _ = syscall.SyscallN(dllMain, mod.Base, 1 /* DLL_PROCESS_ATTACH */, 0)
	}

	// 1. Exécute les callbacks TLS de l'image principale
	if s.Main.Info.TLS != nil {
		for idx, cbRVA := range s.Main.Info.TLS.CallbackRVAs {
			cbAddr := s.Main.Base + uintptr(cbRVA)
			slog.Info("exécution callback TLS", "index", idx, "rva", fmt.Sprintf("0x%X", cbRVA))
			_, _, _ = syscall.SyscallN(cbAddr, s.Main.Base, 1 /* DLL_PROCESS_ATTACH */, 0)
		}
	}

	// 2. Lance le point d'entrée sur un thread OS avec pile 8 MiB
	entryAddr := s.Main.Base + uintptr(s.Main.Info.EntryRVA)
	slog.Info("lancement du point d'entrée natif", "module", s.Main.Name, "entry", fmt.Sprintf("0x%X", entryAddr))

	var tid uint32
	hThread, _, err := procCreateThread.Call(
		0,
		8*1024*1024, // 8 MiB stack
		entryAddr,
		0,
		0,
		uintptr(unsafe.Pointer(&tid)),
	)
	if hThread == 0 {
		return 0, fmt.Errorf("CreateThread(entry) : %w", err)
	}
	defer procCloseHandle.Call(hThread)

	_, _, _ = procWaitForSingleObject.Call(hThread, infiniteTimeout)
	var exitCode uint32
	_, _, _ = procGetExitCodeThread.Call(hThread, uintptr(unsafe.Pointer(&exitCode)))
	return exitCode, nil
}

// Symbolize associe une adresse d'instruction à "module+0xRVA" si elle appartient au titre.
func (s *LiveSession) Symbolize(addr uintptr) string {
	for _, m := range s.Modules {
		if addr >= m.Base && addr < m.Base+uintptr(m.Size) {
			return fmt.Sprintf("%s+0x%X", m.Name, addr-m.Base)
		}
	}
	return fmt.Sprintf("0x%016X", addr)
}

type exceptionRecord struct {
	ExceptionCode        uint32
	ExceptionFlags       uint32
	ExceptionRecord      uintptr
	ExceptionAddress     uintptr
	NumberParameters     uint32
	_pad                 uint32
	ExceptionInformation [15]uintptr
}

type exceptionPointers struct {
	ExceptionRecord *exceptionRecord
	ContextRecord   uintptr
}

func installVEH(sess *LiveSession) {
	vehMu.Lock()
	vehSession = sess
	vehMu.Unlock()

	vehOnce.Do(func() {
		cb := syscall.NewCallback(func(ep *exceptionPointers) uintptr {
			if ep == nil || ep.ExceptionRecord == nil {
				return 0
			}
			code := ep.ExceptionRecord.ExceptionCode
			// Ignore les exceptions bénignes (ex: SetThreadName 0x406D1388, C++ EH 0xE06D7363, DBG_PRINT)
			if code == 0x406D1388 || code == 0xE06D7363 || code == 0x40010006 || code == 0x4001000A {
				return 0
			}
			addr := ep.ExceptionRecord.ExceptionAddress
			// Émulation AMD Zen 2 sur CPU Intel (0xC000001D = STATUS_ILLEGAL_INSTRUCTION) :
			// - CLZERO   (0F 01 FC) : met à zéro la ligne de cache 64 octets pointée par RAX
			// - MONITORX (0F 01 FA) / MWAITX (0F 01 FB) : attente basse consommation (no-op sûr)
			// - RDPRU    (0F 01 FD) : lecture compteur MPERF/APERF -> horloge monotone dans EAX:EDX
			// - CLWB     (66 [REX] 0F AE /6) : cache line write-back hint (no-op sûr sur PC cohérent)
			// - SSE4a    (INSERTQ F2 0F 78/79, EXTRQ 66 0F 78/79) : bitfield insert/extract sur XMM0..XMM15
			if code == 0xC000001D && ep.ContextRecord != 0 && addr > 0x10000 {
				room := 0x1000 - (addr & 0xFFF)
				nRead := room
				if nRead > 16 {
					nRead = 16
				}
				b := unsafe.Slice((*byte)(ptrFromAddr(addr)), nRead)
				ripPtr := (*uint64)(ptrFromAddr(ep.ContextRecord + 0xF8))
				if len(b) >= 3 && b[0] == 0x0F && b[1] == 0x01 {
					switch b[2] {
					case 0xFA, 0xFB: // MONITORX / MWAITX
						*ripPtr += 3
						return ^uintptr(0)
					case 0xFD: // RDPRU
						rcx := uint32(*(*uint64)(ptrFromAddr(ep.ContextRecord + 0x80)))
						var cycles uint64
						if rcx <= 1 {
							cycles = uint64(time.Now().UnixNano())
						}
						*(*uint64)(ptrFromAddr(ep.ContextRecord + 0x78)) = cycles & 0xFFFFFFFF
						*(*uint64)(ptrFromAddr(ep.ContextRecord + 0x88)) = cycles >> 32
						*ripPtr += 3
						return ^uintptr(0)
					case 0xFC: // CLZERO
						raxPtr := (*uint64)(ptrFromAddr(ep.ContextRecord + 0x78))
						start := *raxPtr &^ 63
						if len(b) >= 12 && b[3] == 0x48 && b[4] == 0x83 && b[5] == 0xC0 && b[6] == 0x40 &&
							b[7] == 0x4C && b[8] == 0x39 && b[9] == 0xD0 && b[10] == 0x72 && b[11] == 0xF4 {
							r10 := *(*uint64)(ptrFromAddr(ep.ContextRecord + 0xC8))
							if r10 > start && (r10-start) <= (256<<20) {
								n := ((r10 - start + 63) &^ 63)
								clear(unsafe.Slice((*byte)(ptrFromAddr(uintptr(start))), n))
								*raxPtr = start + n
								*ripPtr += 12
								return ^uintptr(0)
							}
						}
						clear(unsafe.Slice((*byte)(ptrFromAddr(uintptr(start))), 64))
						*ripPtr += 3
						return ^uintptr(0)
					}
				}
				if l := clwbLen(b); l > 0 {
					*ripPtr += uint64(l)
					return ^uintptr(0)
				}
				// XMM0..XMM15 dans Windows x64 CONTEXT : FltSave (+0x100) + XmmRegisters (+0xA0) = +0x1A0
				xmm := (*[16][2]uint64)(ptrFromAddr(ep.ContextRecord + 0x1A0))
				if l := emulateSSE4a(b, xmm); l > 0 {
					*ripPtr += uint64(l)
					return ^uintptr(0)
				}
			}
			vehMu.RLock()
			s := vehSession
			vehMu.RUnlock()

			where := fmt.Sprintf("0x%016X", addr)
			dataDir := ""
			if s != nil {
				where = s.Symbolize(addr)
				dataDir = s.DataDir
			}
			msg := fmt.Sprintf("ASTRAL VEH: exception 0x%08X à %s", code, where)
			if (code == 0xC0000005 || code == 0xC000001D || code == 0x80000003 || code == 0xC0000374) && ep.ContextRecord != 0 {
				if code == 0xC0000005 && ep.ExceptionRecord.NumberParameters >= 2 {
					op := "read"
					if ep.ExceptionRecord.ExceptionInformation[0] == 1 {
						op = "write"
					}
					msg = fmt.Sprintf("%s (%s @ 0x%016X)", msg, op, ep.ExceptionRecord.ExceptionInformation[1])
				}
				if addr > 0x10000 {
					codeBytes := unsafe.Slice((*byte)(ptrFromAddr(addr)), 16)
					msg = fmt.Sprintf("%s bytes=% X", msg, codeBytes)
				}
				// Offsets Windows x64 CONTEXT : Rax=+0x78, Rcx=+0x80, Rdx=+0x88, Rbx=+0x90, Rsp=+0x98, R8=+0xB8, R9=+0xC0
				rax := *(*uint64)(ptrFromAddr(ep.ContextRecord + 0x78))
				rcx := *(*uint64)(ptrFromAddr(ep.ContextRecord + 0x80))
				rdx := *(*uint64)(ptrFromAddr(ep.ContextRecord + 0x88))
				rbx := *(*uint64)(ptrFromAddr(ep.ContextRecord + 0x90))
				r8 := *(*uint64)(ptrFromAddr(ep.ContextRecord + 0xB8))
				r9 := *(*uint64)(ptrFromAddr(ep.ContextRecord + 0xC0))
				rsp := *(*uint64)(ptrFromAddr(ep.ContextRecord + 0x98))
				msg = fmt.Sprintf("%s rax=0x%X rcx=0x%X rdx=0x%X rbx=0x%X r8=0x%X r9=0x%X", msg, rax, rcx, rdx, rbx, r8, r9)
				if s != nil && rsp > 0x10000 {
					var callers []string
					stackWords := unsafe.Slice((*uintptr)(ptrFromAddr(uintptr(rsp))), 512)
					for _, w := range stackWords {
						for _, m := range s.Modules {
							if w >= m.Base && w < m.Base+uintptr(m.Size) {
								callers = append(callers, fmt.Sprintf("%s+0x%X", m.Name, w-m.Base))
								break
							}
						}
						if len(callers) >= 12 {
							break
						}
					}
					if len(callers) > 0 {
						msg = fmt.Sprintf("%s stack=[%s]", msg, strings.Join(callers, ", "))
					}
				}
			}
			slog.Error(msg)
			if dataDir != "" {
				_ = AppendCrashLog(dataDir, msg)
			}
			return 0 // EXCEPTION_CONTINUE_SEARCH
		})
		_, _, _ = procAddVectoredExHandler.Call(0, cb)
	})
}
