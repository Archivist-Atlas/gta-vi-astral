// Package gpu regroupe les contrats ABI Xbox Series D3D12.X et l'abstraction
// double-backend (D3D12 hôte façon VIctory et Vulkan + XDXR BVH4 façon Leonida).
package gpu

// Slots vtable Xbox Series D3D12.X mesurés empiriquement par VIctory et Leonida.
const (
	// ID3D12Device Xbox Series
	SlotDeviceGetCustomHeapProperties = 26  // Quirk ABI : (return_buf, this, ...) inversé par rapport à MSVC PC
	SlotDeviceCreateCommandQueueX     = 48
	SlotDeviceCreateComputePSO        = 62  // Contient 2 mots supplémentaires par rapport à PC
	SlotDeviceCreateNativeComputePSO  = 67
	SlotDeviceCreateCommandListX      = 73  // IID console spécifique
	SlotDeviceCreateResourceX         = 76  // Mappe vers CreateCommittedResource (layout 0x101/0x1 -> PC)
	SlotDeviceSetFrameIntervalX       = 78
	SlotDeviceScheduleFrameEventX     = 79
	SlotDeviceWaitFrameEventX         = 80
	SlotDeviceCreatePlacedResourceX   = 133 // Prend une VA brute et state = 0xFFFFFFFF

	// ID3D12DescriptorHeap Xbox Series
	SlotHeapGetCPUHandleStart = 9  // Retourne le handle 8 octets dans RAX (et non via pointeur caché RDX)
	SlotHeapGetGPUHandleStart = 10 // Idem en RAX

	// ID3D12CommandQueue Xbox Series
	SlotQueuePresentXPrimary   = 30
	SlotQueuePresentXAlternate = 32

	// ID3D12GraphicsCommandList Xbox Series
	SlotListAllocateUploadMemory = 148 // Arène d'upload mappée -> BDA

	// Drapeaux de root signature console à nettoyer avant sérialisation PC
	XboxRootSigFlagMask uint32 = 0xFF000000
)

// SanitizeRootSignatureFlags retire l'octet haut réservé Xbox Series (ex: 0x03000000)
// afin que la root signature soit acceptée par D3D12 PC et Vulkan.
func SanitizeRootSignatureFlags(flags uint32) uint32 {
	return flags &^ XboxRootSigFlagMask
}

// InlineCommandListTrap décrit l'astuce d'interception sans patch binaire (VIctory & Leonida) :
// en maintenant PutPointer == LimitPointer == 0, tout code inliné du jeu tombe dans la vtable.
type InlineCommandListTrap struct {
	PutPointer   uintptr
	LimitPointer uintptr
}

// NewInlineCommandListTrap retourne un état garantissant l'interception par la vtable.
func NewInlineCommandListTrap() InlineCommandListTrap {
	return InlineCommandListTrap{PutPointer: 0, LimitPointer: 0}
}
