package hle

// Drapeaux mémoire spécifiques Xbox Series X|S relevés par VIctory et Leonida.
const (
	MemGraphicsFlag         uint32 = 0x10000000
	MemTitleFlag            uint32 = 0x40000000
	MemReservePlaceholder   uint32 = 0x00040000
	MemReplacePlaceholder   uint32 = 0x00004000
	PageGraphicsReadOnly    uint32 = 0x00001000
	PageGraphicsReadWrite   uint32 = 0x00002000
	PageGraphicsExecute     uint32 = 0x00004000
	PageGraphicsCoherent    uint32 = 0x00020000
	PageGraphicsNoCache     uint32 = 0x00040000
)

// SanitizeAllocFlags retire les drapeaux d'allocation propres à la console
// avant de transmettre l'appel à VirtualAlloc sur Windows PC.
func SanitizeAllocFlags(flags uint32) uint32 {
	return flags &^ (MemGraphicsFlag | MemTitleFlag)
}

// SanitizePageProtect traduit une protection mémoire console (PAGE_GRAPHICS_*)
// vers une protection Win32 standard équivalente.
func SanitizePageProtect(protect uint32) uint32 {
	const (
		pageReadOnly  = 0x02
		pageReadWrite = 0x04
		pageExecRW    = 0x40
	)
	clean := protect &^ (PageGraphicsReadOnly | PageGraphicsReadWrite | PageGraphicsExecute | PageGraphicsCoherent | PageGraphicsNoCache)
	if clean != 0 {
		return clean
	}
	switch {
	case protect&PageGraphicsExecute != 0:
		return pageExecRW
	case protect&PageGraphicsReadWrite != 0:
		return pageReadWrite
	case protect&PageGraphicsReadOnly != 0:
		return pageReadOnly
	default:
		return pageReadWrite
	}
}
