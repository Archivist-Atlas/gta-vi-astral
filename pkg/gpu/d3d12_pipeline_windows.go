//go:build windows

package gpu

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	d3dCompileMu sync.Mutex
	psoCreateMu  sync.Mutex
)

var (
	modD3DCompiler47          = syscall.NewLazyDLL("d3dcompiler_47.dll")
	procD3DCompile            = modD3DCompiler47.NewProc("D3DCompile")
	procD3D12SerializeRootSig = modD3D12Host.NewProc("D3D12SerializeRootSignature")

	// IID_ID3D12RootSignature = {c54a6b66-72df-4ee8-8be5-a946a1429214}
	guidID3D12RootSignature = [16]byte{
		0x66, 0x6b, 0x4a, 0xc5, 0xdf, 0x72, 0xe8, 0x4e,
		0x8b, 0xe5, 0xa9, 0x46, 0xa1, 0x42, 0x92, 0x14,
	}
	// IID_ID3D12PipelineState = {765a30f3-f624-4c6f-a828-ace948622445}
	guidID3D12PipelineState = [16]byte{
		0xf3, 0x30, 0x5a, 0x76, 0x24, 0xf6, 0x6f, 0x4c,
		0xa8, 0x28, 0xac, 0xe9, 0x48, 0x62, 0x24, 0x45,
	}
)

// HostTextureUpload décrit une texture 2D invitée à uploader dans un SRV t0 hôte.
type HostTextureUpload struct {
	GuestVA   uint64
	Width     uint32
	Height    uint32
	MipLevels uint32
	Format    uint32 // DXGI_FORMAT
	Swizzle   uint32 // Layout RDNA2 (0=linéaire, 0x105=SW_4KB_S, 0x109+=SW_64KB_*)
	Pixels    []byte
}

// HostDrawPass décrit un appel DrawInstanced ou DrawIndexedInstanced rejoué sur le GPU hôte.
type HostDrawPass struct {
	IsClear       bool
	ClearColor    [4]float32
	Indexed       bool
	Topology      uint32 // 4=TRIANGLELIST, 5=TRIANGLESTRIP, 17/32=Xbox RECTLIST
	VertexCount   uint32 // VertexCountPerInstance ou IndexCountPerInstance
	InstanceCount uint32
	StartVertex   uint32 // StartVertexLocation ou StartIndexLocation
	BaseVertex    int32
	StartInstance uint32
	PassIndex     uint32
	FrameNumber   uint32
	HostPSO       uintptr
	TargetVA      uint64 // Adresse GPU invitée du RenderTarget (0 ou presentVA = backbuffer)
	SRVGuestVA       uint64 // Adresse GPU invitée du SRV t0 (pour liaison directe RenderTarget -> SRV)
	ExtraSRVGuestVAs [3]uint64
	VBData           []byte
	VBStride      uint32
	IBData        []byte
	IBFormat      uint32 // 57 = R16_UINT, 42 = R32_UINT
	CBData        []byte
	PSCBData      []byte
	SRVTex        *HostTextureUpload
	ExtraSRVs     [3]*HostTextureUpload
}

type hostRTEntry struct {
	res       uintptr
	rtvCPU    uintptr
	srvGPU    uintptr
	width     uint32
	height    uint32
	state     uint32 // 0x4 = RENDER_TARGET, 0x80 = PIXEL_SHADER_RESOURCE
	initClear bool
}

type d3d12IndexBufferView struct {
	BufferLocation uint64
	SizeInBytes    uint32
	Format         uint32
}

type hostTexEntry struct {
	res      uintptr
	width    uint32
	height   uint32
	format   uint32
	srvGPU   uintptr
	hash     uint64
	uploaded bool
}

type d3d12VertexBufferView struct {
	BufferLocation uint64
	SizeInBytes    uint32
	StrideInBytes  uint32
}

type d3d12ShaderBytecode struct {
	Ptr uintptr
	Len uintptr
}

type d3d12StreamOutputDesc struct {
	SODeclaration    uintptr
	NumEntries       uint32
	_pad0            uint32
	BufferStrides    uintptr
	NumStrides       uint32
	RasterizedStream uint32
}

type d3d12RenderTargetBlendDesc struct {
	BlendEnable           int32
	LogicOpEnable         int32
	SrcBlend              uint32
	DestBlend             uint32
	BlendOp               uint32
	SrcBlendAlpha         uint32
	DestBlendAlpha        uint32
	BlendOpAlpha          uint32
	LogicOp               uint32
	RenderTargetWriteMask uint8
	_pad                  [3]byte
}

type d3d12BlendDesc struct {
	AlphaToCoverageEnable  int32
	IndependentBlendEnable int32
	RenderTarget           [8]d3d12RenderTargetBlendDesc
}

type d3d12RasterizerDesc struct {
	FillMode              uint32
	CullMode              uint32
	FrontCounterClockwise int32
	DepthBias             int32
	DepthBiasClamp        float32
	SlopeScaledDepthBias  float32
	DepthClipEnable       int32
	MultisampleEnable     int32
	AntialiasedLineEnable int32
	ForcedSampleCount     uint32
	ConservativeRaster    uint32
}

type d3d12DepthStencilOpDesc struct {
	StencilFailOp      uint32
	StencilDepthFailOp uint32
	StencilPassOp      uint32
	StencilFunc        uint32
}

type d3d12DepthStencilDesc struct {
	DepthEnable      int32
	DepthWriteMask   uint32
	DepthFunc        uint32
	StencilEnable    int32
	StencilReadMask  uint8
	StencilWriteMask uint8
	_pad             [2]byte
	FrontFace        d3d12DepthStencilOpDesc
	BackFace         d3d12DepthStencilOpDesc
}

type d3d12InputLayoutDesc struct {
	Elements    uintptr
	NumElements uint32
	_pad        uint32
}

type d3d12SampleDesc struct {
	Count   uint32
	Quality uint32
}

type d3d12GraphicsPSODesc struct {
	RootSignature         uintptr
	VS                    d3d12ShaderBytecode
	PS                    d3d12ShaderBytecode
	DS                    d3d12ShaderBytecode
	HS                    d3d12ShaderBytecode
	GS                    d3d12ShaderBytecode
	StreamOutput          d3d12StreamOutputDesc
	BlendState            d3d12BlendDesc
	SampleMask            uint32
	RasterizerState       d3d12RasterizerDesc
	DepthStencilState     d3d12DepthStencilDesc
	InputLayout           d3d12InputLayoutDesc
	IBStripCutValue       uint32
	PrimitiveTopologyType uint32
	NumRenderTargets      uint32
	RTVFormats            [8]uint32
	DSVFormat             uint32
	SampleDesc            d3d12SampleDesc
	NodeMask              uint32
	CachedPSO             d3d12ShaderBytecode
	Flags                 uint32
}

type d3d12DescriptorRange struct {
	RangeType                         uint32 // 0 = SRV
	NumDescriptors                    uint32
	BaseShaderRegister                uint32
	RegisterSpace                     uint32
	OffsetInDescriptorsFromTableStart uint32
}

// d3d12RootParameter (32 octets sur x64 : Type u32, _pad u32, union 16B, Visibility u32, _pad u32)
type d3d12RootParameter struct {
	ParameterType    uint32
	_pad             uint32
	Union0           uint64 // NumDescriptorRanges / ShaderRegister|RegisterSpace
	Union1           uint64 // pDescriptorRanges / Num32BitValues
	ShaderVisibility uint32
	_padTail         uint32
}

type d3d12StaticSamplerDesc struct {
	Filter           uint32
	AddressU         uint32
	AddressV         uint32
	AddressW         uint32
	MipLODBias       float32
	MaxAnisotropy    uint32
	ComparisonFunc   uint32
	BorderColor      uint32
	MinLOD           float32
	MaxLOD           float32
	ShaderRegister   uint32
	RegisterSpace    uint32
	ShaderVisibility uint32
}

type d3d12RootSignatureDesc struct {
	NumParameters     uint32
	_pad0             uint32
	Parameters        uintptr
	NumStaticSamplers uint32
	_pad1             uint32
	StaticSamplers    uintptr
	Flags             uint32
	_pad2             uint32
}

type d3d12SRVDesc struct {
	Format                  uint32
	ViewDimension           uint32 // 4 = TEXTURE2D
	Shader4ComponentMapping uint32 // 0x1688 = D3D12_DEFAULT_SHADER_4_COMPONENT_MAPPING
	_pad                    uint32 // alignement 8 octets de l'union (D3D12_BUFFER_SRV::FirstElement est UINT64)
	MostDetailedMip         uint32
	MipLevels               uint32
	PlaneSlice              uint32
	ResourceMinLODClamp     float32
}

type d3d12Viewport struct {
	TopLeftX float32
	TopLeftY float32
	Width    float32
	Height   float32
	MinDepth float32
	MaxDepth float32
}

type d3d12Rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

// CompileHLSL compile un shader HLSL en bytecode DXBC via d3dcompiler_47.dll (natif Windows).
func CompileHLSL(source, entryPoint, target string) ([]byte, error) {
	if err := procD3DCompile.Find(); err != nil {
		return nil, fmt.Errorf("d3dcompiler_47.dll indisponible : %w", err)
	}
	srcBytes := []byte(source)
	entryBytes := append([]byte(entryPoint), 0)
	targetBytes := append([]byte(target), 0)
	var flags1 uintptr
	if len(source) > 8192 {
		// Évite 5-20s de blocage D3DCompile sur les gros shaders 3D/Compute -> prévient le watchdog RAGE.
		flags1 = (1 << 2) | (1 << 14) // D3DCOMPILE_SKIP_OPTIMIZATION | D3DCOMPILE_OPTIMIZATION_LEVEL0
	}
	var codeBlob, errBlob uintptr
	// d3dcompiler_47.dll (FXC) n'est pas thread-safe sous forte concurrence sur de gros shaders (5000+ ins) :
	// sérialisation stricte + KeepAlive pour éviter les déréférencements NULL internes (0xC0000005).
	d3dCompileMu.Lock()
	hr, _, _ := procD3DCompile.Call(
		uintptr(unsafe.Pointer(&srcBytes[0])),
		uintptr(len(srcBytes)),
		0, 0, 0,
		uintptr(unsafe.Pointer(&entryBytes[0])),
		uintptr(unsafe.Pointer(&targetBytes[0])),
		flags1, 0,
		uintptr(unsafe.Pointer(&codeBlob)),
		uintptr(unsafe.Pointer(&errBlob)),
	)
	d3dCompileMu.Unlock()
	runtime.KeepAlive(srcBytes)
	runtime.KeepAlive(entryBytes)
	runtime.KeepAlive(targetBytes)
	if errBlob != 0 {
		defer comCall(errBlob, 2) // Release
	}
	if int32(hr) < 0 || codeBlob == 0 {
		var msg string
		if errBlob != 0 {
			ptr := comCall(errBlob, 3) // GetBufferPointer
			sz := comCall(errBlob, 4)  // GetBufferSize
			if ptr != 0 && sz > 0 && sz < 8192 {
				msg = string(unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&ptr))), sz))
			}
		}
		return nil, fmt.Errorf("D3DCompile(%s/%s) échoué (0x%08X): %s", entryPoint, target, uint32(hr), msg)
	}
	defer comCall(codeBlob, 2) // Release
	ptr := comCall(codeBlob, 3)
	sz := comCall(codeBlob, 4)
	out := make([]byte, sz)
	copy(out, unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&ptr))), sz))
	return out, nil
}

// HostInputElement décrit un attribut vertex pour D3D12_INPUT_ELEMENT_DESC.
type HostInputElement struct {
	SemanticName      string
	SemanticIndex     uint32
	Format            uint32 // DXGI_FORMAT (ex. 28 = R8G8B8A8_UNORM, 16 = R32G32_FLOAT)
	InputSlot         uint32
	AlignedByteOffset uint32
	HasOffset         bool
}

type d3d12InputElementDesc struct {
	SemanticName         uintptr
	SemanticIndex        uint32
	Format               uint32
	InputSlot            uint32
	AlignedByteOffset    uint32
	InputSlotClass       uint32
	InstanceDataStepRate uint32
}

// HostBlendState décrit l'état de fusion (RenderTarget[0]) extrait du PSO invité.
type HostBlendState struct {
	BlendEnable    bool
	SrcBlend       uint32
	DestBlend      uint32
	BlendOp        uint32
	SrcBlendAlpha  uint32
	DestBlendAlpha uint32
	BlendOpAlpha   uint32
	WriteMask      uint8
}

// CompileGraphicsPSO compile une paire HLSL (VS + PS) et crée un ID3D12PipelineState matériel sur h.Device.
func (h *HostD3D12) CompileGraphicsPSO(vsHLSL, vsEntry, psHLSL, psEntry string, inputElems []HostInputElement) (uintptr, error) {
	return h.CompileGraphicsPSOWithBlend(vsHLSL, vsEntry, psHLSL, psEntry, inputElems, nil)
}

// CompileGraphicsPSOWithBlend compile une paire HLSL (VS + PS) avec l'état de fusion invité (ou alpha blending par défaut).
func (h *HostD3D12) CompileGraphicsPSOWithBlend(vsHLSL, vsEntry, psHLSL, psEntry string, inputElems []HostInputElement, blend *HostBlendState) (uintptr, error) {
	if h == nil || h.Device == 0 || h.ReplayRootSig == 0 {
		return 0, fmt.Errorf("HostD3D12 non initialisé")
	}
	vsBC, err := CompileHLSL(vsHLSL, vsEntry, "vs_5_0")
	if err != nil {
		return 0, err
	}
	psBC, err := CompileHLSL(psHLSL, psEntry, "ps_5_0")
	if err != nil {
		return 0, err
	}
	return h.createGraphicsPSOFromDXBC(vsBC, psBC, inputElems, blend)
}

func (h *HostD3D12) createGraphicsPSOFromDXBC(vsBC, psBC []byte, inputElems []HostInputElement, blend *HostBlendState) (uintptr, error) {
	var rawDescs []d3d12InputElementDesc
	var nameBufs [][]byte
	if len(inputElems) > 0 {
		rawDescs = make([]d3d12InputElementDesc, len(inputElems))
		nameBufs = make([][]byte, len(inputElems))
		for i, el := range inputElems {
			nb := append([]byte(el.SemanticName), 0)
			nameBufs[i] = nb
			fmtID := el.Format
			if fmtID == 0 {
				fmtID = 2 // DXGI_FORMAT_R32G32B32A32_FLOAT
			}
			byteOff := uint32(0xFFFFFFFF) // D3D12_APPEND_ALIGNED_ELEMENT
			if el.HasOffset {
				byteOff = el.AlignedByteOffset
			}
			rawDescs[i] = d3d12InputElementDesc{
				SemanticName:      uintptr(unsafe.Pointer(&nb[0])),
				SemanticIndex:     el.SemanticIndex,
				Format:            fmtID,
				InputSlot:         el.InputSlot,
				AlignedByteOffset: byteOff,
			}
		}
	}
	var ilDesc d3d12InputLayoutDesc
	if len(rawDescs) > 0 {
		ilDesc = d3d12InputLayoutDesc{
			Elements:    uintptr(unsafe.Pointer(&rawDescs[0])),
			NumElements: uint32(len(rawDescs)),
		}
	}
	psoDesc := d3d12GraphicsPSODesc{
		RootSignature: h.ReplayRootSig,
		VS:            d3d12ShaderBytecode{Ptr: uintptr(unsafe.Pointer(&vsBC[0])), Len: uintptr(len(vsBC))},
		PS:            d3d12ShaderBytecode{Ptr: uintptr(unsafe.Pointer(&psBC[0])), Len: uintptr(len(psBC))},
		InputLayout:   ilDesc,
		SampleMask:    0xFFFFFFFF,
		RasterizerState: d3d12RasterizerDesc{
			FillMode:        3, // SOLID
			CullMode:        1, // NONE
			DepthClipEnable: 1,
		},
		DepthStencilState: d3d12DepthStencilDesc{
			DepthEnable:    0,
			DepthWriteMask: 0,
			DepthFunc:      8, // ALWAYS
		},
		PrimitiveTopologyType: 3, // TRIANGLE
		NumRenderTargets:      1,
		SampleDesc:            d3d12SampleDesc{Count: 1, Quality: 0},
	}
	if blend != nil {
		en := int32(0)
		if blend.BlendEnable {
			en = 1
		}
		mask := blend.WriteMask
		if mask == 0 {
			mask = 0x0F
		}
		psoDesc.BlendState.RenderTarget[0] = d3d12RenderTargetBlendDesc{
			BlendEnable:           en,
			SrcBlend:              blend.SrcBlend,
			DestBlend:             blend.DestBlend,
			BlendOp:               blend.BlendOp,
			SrcBlendAlpha:         blend.SrcBlendAlpha,
			DestBlendAlpha:        blend.DestBlendAlpha,
			BlendOpAlpha:          blend.BlendOpAlpha,
			RenderTargetWriteMask: mask,
		}
	} else {
		psoDesc.BlendState.RenderTarget[0] = d3d12RenderTargetBlendDesc{
			BlendEnable:           1, // Alpha blending standard (SRC_ALPHA / INV_SRC_ALPHA) par défaut
			SrcBlend:              5, // D3D12_BLEND_SRC_ALPHA
			DestBlend:             6, // D3D12_BLEND_INV_SRC_ALPHA
			BlendOp:               1, // ADD
			SrcBlendAlpha:         2, // ONE
			DestBlendAlpha:        6, // INV_SRC_ALPHA
			BlendOpAlpha:          1, // ADD
			RenderTargetWriteMask: 0x0F,
		}
	}
	psoDesc.RTVFormats[0] = 28 // DXGI_FORMAT_R8G8B8A8_UNORM

	var pso uintptr
	// ID3D12Device::CreateGraphicsPipelineState = slot 10
	psoCreateMu.Lock()
	hr := comCall(h.Device, 10, uintptr(unsafe.Pointer(&psoDesc)), uintptr(unsafe.Pointer(&guidID3D12PipelineState[0])), uintptr(unsafe.Pointer(&pso)))
	psoCreateMu.Unlock()
	runtime.KeepAlive(vsBC)
	runtime.KeepAlive(psBC)
	runtime.KeepAlive(rawDescs)
	runtime.KeepAlive(nameBufs)
	runtime.KeepAlive(&psoDesc)
	if int32(hr) < 0 || pso == 0 {
		h.dumpDebugMessages()
		return 0, fmt.Errorf("CreateGraphicsPipelineState échoué (0x%08X, removedReason=0x%08X, inElems=%d)", uint32(hr), h.DeviceRemovedReason(), len(inputElems))
	}
	return pso, nil
}

type d3d12ComputePSODesc struct {
	RootSignature uintptr
	CS            d3d12ShaderBytecode
	NodeMask      uint32
	_pad0         uint32
	CachedPSO     d3d12ShaderBytecode
	Flags         uint32
	_pad1         uint32
}

// HostDispatchPass décrit un appel Dispatch Compute rejoué sur le GPU Windows hôte.
type HostDispatchPass struct {
	X       uint32
	Y       uint32
	Z       uint32
	HostPSO uintptr
	CBData  []byte
}

// CompileComputePSO compile un shader HLSL Compute (cs_5_0) en ID3D12PipelineState matériel sur h.Device.
func (h *HostD3D12) CompileComputePSO(csHLSL, csEntry string) (uintptr, error) {
	if h == nil || h.Device == 0 || h.ComputeRootSig == 0 {
		return 0, fmt.Errorf("HostD3D12 Compute non initialisé")
	}
	csBC, err := CompileHLSL(csHLSL, csEntry, "cs_5_0")
	if err != nil {
		return 0, err
	}
	desc := d3d12ComputePSODesc{
		RootSignature: h.ComputeRootSig,
		CS:            d3d12ShaderBytecode{Ptr: uintptr(unsafe.Pointer(&csBC[0])), Len: uintptr(len(csBC))},
	}
	var pso uintptr
	// ID3D12Device::CreateComputePipelineState = slot 11
	hr := comCall(h.Device, 11, uintptr(unsafe.Pointer(&desc)), uintptr(unsafe.Pointer(&guidID3D12PipelineState[0])), uintptr(unsafe.Pointer(&pso)))
	if int32(hr) < 0 || pso == 0 {
		return 0, fmt.Errorf("CreateComputePipelineState échoué (0x%08X)", uint32(hr))
	}
	return pso, nil
}

// CompileHostComputePSO compile un Compute Shader HLSL (cs_5_0) sur l'instance HostD3D12 globale.
func CompileHostComputePSO(csHLSL, csEntry string) (uintptr, error) {
	hostMu.Lock()
	h := hostInstance
	hostMu.Unlock()
	if h == nil {
		return 0, fmt.Errorf("HostD3D12 inactif")
	}
	return h.CompileComputePSO(csHLSL, csEntry)
}

// CompileHostPSO compile une paire HLSL (VS+PS) en ID3D12PipelineState sur l'instance HostD3D12 globale.
func CompileHostPSO(vsHLSL, vsEntry, psHLSL, psEntry string, inputElems []HostInputElement) (uintptr, error) {
	return CompileHostPSOWithBlend(vsHLSL, vsEntry, psHLSL, psEntry, inputElems, nil)
}

func RegisterGammaCompositePSO(pso uintptr) {
	hostMu.Lock()
	if hostInstance != nil {
		hostInstance.GammaCompositePSO = pso
	}
	hostMu.Unlock()
}

func CompileHostPSOWithBlend(vsHLSL, vsEntry, psHLSL, psEntry string, inputElems []HostInputElement, blend *HostBlendState) (uintptr, error) {
	hostMu.Lock()
	h := hostInstance
	hostMu.Unlock()
	if h == nil {
		return 0, fmt.Errorf("HostD3D12 inactif")
	}
	return h.CompileGraphicsPSOWithBlend(vsHLSL, vsEntry, psHLSL, psEntry, inputElems, blend)
}

// initReplayPipeline crée la RootSignature hôte (b0 16 uints + t0 SRV + s0 static sampler),
// un heap SRV shader-visible avec descripteur 2D par défaut, et le PSO plein écran de repli.
func (h *HostD3D12) initReplayPipeline() {
	if h.Device == 0 {
		return
	}
	if err := procD3D12SerializeRootSig.Find(); err != nil {
		return
	}

	// Param 0 : Root CBV b0 (VS/CS), Param 1 : SRV t0, Param 2 : Root CBV b1 (PS),
	// Params 3..5 : SRV t1..t3 (textures multi-plans YUV / UI / dither)
	var srvRanges [4]d3d12DescriptorRange
	for i := uint32(0); i < 4; i++ {
		srvRanges[i] = d3d12DescriptorRange{
			RangeType:                         0, // SRV
			NumDescriptors:                    1,
			BaseShaderRegister:                i,
			RegisterSpace:                     0,
			OffsetInDescriptorsFromTableStart: 0,
		}
	}
	params := [6]d3d12RootParameter{
		{ParameterType: 2 /* CBV */, Union0: 0 /* b0 */, ShaderVisibility: 0},
		{ParameterType: 0 /* TABLE */, Union0: 1, Union1: uint64(uintptr(unsafe.Pointer(&srvRanges[0]))), ShaderVisibility: 0},
		{ParameterType: 2 /* CBV */, Union0: 1 /* b1 */, ShaderVisibility: 0},
		{ParameterType: 0 /* TABLE */, Union0: 1, Union1: uint64(uintptr(unsafe.Pointer(&srvRanges[1]))), ShaderVisibility: 0},
		{ParameterType: 0 /* TABLE */, Union0: 1, Union1: uint64(uintptr(unsafe.Pointer(&srvRanges[2]))), ShaderVisibility: 0},
		{ParameterType: 0 /* TABLE */, Union0: 1, Union1: uint64(uintptr(unsafe.Pointer(&srvRanges[3]))), ShaderVisibility: 0},
	}
	sampler := d3d12StaticSamplerDesc{
		Filter:           0x15, // MIN_MAG_MIP_LINEAR
		AddressU:         3,    // CLAMP
		AddressV:         3,    // CLAMP
		AddressW:         3,    // CLAMP
		MaxAnisotropy:    1,
		ComparisonFunc:   8, // ALWAYS
		MaxLOD:           3.402823466e+38,
		ShaderRegister:   0,
		RegisterSpace:    0,
		ShaderVisibility: 0, // ALL
	}
	rsDesc := d3d12RootSignatureDesc{
		NumParameters:     6,
		Parameters:        uintptr(unsafe.Pointer(&params[0])),
		NumStaticSamplers: 1,
		StaticSamplers:    uintptr(unsafe.Pointer(&sampler)),
		Flags:             1, // D3D12_ROOT_SIGNATURE_FLAG_ALLOW_INPUT_ASSEMBLER_INPUT_LAYOUT
	}
	var rsBlob, errBlob uintptr
	hr, _, _ := procD3D12SerializeRootSig.Call(
		uintptr(unsafe.Pointer(&rsDesc)),
		1, // D3D_ROOT_SIGNATURE_VERSION_1
		uintptr(unsafe.Pointer(&rsBlob)),
		uintptr(unsafe.Pointer(&errBlob)),
	)
	if errBlob != 0 {
		_ = comCall(errBlob, 2)
	}
	if int32(hr) < 0 || rsBlob == 0 {
		slog.Warn("Host D3D12 SerializeRootSignature échoué", "hr", fmt.Sprintf("0x%08X", uint32(hr)))
		return
	}
	defer comCall(rsBlob, 2)

	rsPtr := comCall(rsBlob, 3)
	rsSize := comCall(rsBlob, 4)
	var rootSig uintptr
	hr = comCall(h.Device, 16, 0, rsPtr, rsSize, uintptr(unsafe.Pointer(&guidID3D12RootSignature[0])), uintptr(unsafe.Pointer(&rootSig)))
	if int32(hr) < 0 || rootSig == 0 {
		slog.Warn("Host D3D12 CreateRootSignature échoué", "hr", fmt.Sprintf("0x%08X", uint32(hr)))
		return
	}
	h.ReplayRootSig = rootSig

	// Heap SRV shader-visible (2048 descripteurs : slot 0 = null-descriptor par défaut, slots 1..2047 = textures 2D & RT hôtes)
	srvHeapDesc := d3d12DescriptorHeapDesc{Type: 0 /* CBV_SRV_UAV */, NumDescriptors: 2048, Flags: 1 /* SHADER_VISIBLE */}
	if int32(comCall(h.Device, 14, uintptr(unsafe.Pointer(&srvHeapDesc)), uintptr(unsafe.Pointer(&guidID3D12DescriptorHeap[0])), uintptr(unsafe.Pointer(&h.SRVHeap)))) >= 0 && h.SRVHeap != 0 {
		var cpuStart, gpuStart uintptr
		retCPU := comCall(h.SRVHeap, 9, uintptr(unsafe.Pointer(&cpuStart)))  // GetCPUDescriptorHandleForHeapStart
		retGPU := comCall(h.SRVHeap, 10, uintptr(unsafe.Pointer(&gpuStart))) // GetGPUDescriptorHandleForHeapStart
		if cpuStart == 0 {
			cpuStart = retCPU
		}
		if gpuStart == 0 {
			gpuStart = retGPU
		}
		h.SRVCPUStart = cpuStart
		h.SRVGPUStart = gpuStart
		h.SRVStride = comCall(h.Device, 15, 0 /* CBV_SRV_UAV */)
		slog.Info("Host D3D12 SRVHeap initialisé", "cpuStart", fmt.Sprintf("0x%X", cpuStart), "gpuStart", fmt.Sprintf("0x%X", gpuStart), "stride", h.SRVStride)
		h.NextSRVSlot = 1
		h.TexCache = make(map[uint64]*hostTexEntry)
		h.RTCache = make(map[uint64]*hostRTEntry)
		h.NextRTVSlot = 2
		srvDesc := d3d12SRVDesc{
			Format:                  28,     // R8G8B8A8_UNORM
			ViewDimension:           4,      // TEXTURE2D
			Shader4ComponentMapping: 0x1688, // DEFAULT
			MipLevels:               1,
		}
		// ID3D12Device::CreateShaderResourceView (slot 18) avec pResource=NULL (null descriptor D3D12 légal)
		_ = comCall(h.Device, 18, 0, uintptr(unsafe.Pointer(&srvDesc)), cpuStart)
	}

	const hlslSrc = `
cbuffer RootData : register(b0) {
    uint4 RootConst[64];
};
struct VSOut {
    float4 pos : SV_Position;
    float2 uv  : TEXCOORD0;
};
VSOut VSMain(uint id : SV_VertexID) {
    VSOut o;
    o.uv = float2((id << 1) & 2, id & 2);
    o.pos = float4(o.uv * float2(2.0, -2.0) + float2(-1.0, 1.0), 0.0, 1.0);
    return o;
}
float4 PSMain(VSOut i) : SV_Target {
    float t = float(RootConst[0].x) * 0.03;
    float pulse = 0.08 + 0.04 * sin(t + float(RootConst[0].y) * 1.1);
    float3 base = float3(0.04 + pulse * 0.4, 0.05 + pulse * 0.5, 0.10 + pulse);
    float vign = 1.0 - 0.35 * dot(i.uv - 0.5, i.uv - 0.5);
    return float4(base * vign, 1.0);
}
`
	pso, err := h.CompileGraphicsPSO(hlslSrc, "VSMain", hlslSrc, "PSMain", nil)
	if err != nil {
		slog.Warn("Host D3D12 PSO de repli échoué", "err", err)
		return
	}
	h.ReplayPSO = pso
	yuvHLSL := `
Texture2D<float4> TexY : register(t0);
Texture2D<float4> TexU : register(t1);
Texture2D<float4> TexV : register(t2);
SamplerState Samp0 : register(s0);
struct VSOut { float4 pos : SV_Position; float2 uv : TEXCOORD0; };
VSOut VSMain(uint id : SV_VertexID) {
    VSOut o;
    o.uv = float2((id << 1) & 2, id & 2);
    o.pos = float4(o.uv * float2(2.0, -2.0) + float2(-1.0, 1.0), 0.0, 1.0);
    return o;
}
float4 PSMain(VSOut i) : SV_Target {
    float y = TexY.Sample(Samp0, i.uv).r;
    // Bink2 (psSRVs=[2,1,3,4]) : t0=Y, t1=Cr(V), t2=Cb(U)
    float v = TexU.Sample(Samp0, i.uv).r - 0.5;
    float u = TexV.Sample(Samp0, i.uv).r - 0.5;
    float yy = (y - 16.0/255.0) * (255.0/219.0);
    float r = saturate(yy + 1.5748 * v);
    float g = saturate(yy - 0.1873 * u - 0.4681 * v);
    float b = saturate(yy + 1.8556 * u);
    return float4(r, g, b, 1.0);
}
`
	if yuvPSO, yerr := h.CompileGraphicsPSO(yuvHLSL, "VSMain", yuvHLSL, "PSMain", nil); yerr == nil && yuvPSO != 0 {
		h.YUVPSO = yuvPSO
	}
	// Buffers dynamiques persistants UPLOAD pour VB (32 MiB), IB (16 MiB), CB (16 MiB = 1024x16 KiB) et Staging Texture (32 MiB)
	h.UploadVB, h.UploadVBPtr, h.UploadVBGPU = h.createUploadBuffer(32 << 20)
	h.UploadIB, h.UploadIBPtr, h.UploadIBGPU = h.createUploadBuffer(16 << 20)
	h.UploadCB, h.UploadCBPtr, h.UploadCBGPU = h.createUploadBuffer(16 << 20)
	h.UploadTex, h.UploadTexPtr, _ = h.createUploadBuffer(32 << 20)
	h.initComputePipeline()
	slog.Info("Host D3D12 Replay PSO matériel compilé (VS+PS+CS DXBC, RootCBV b0 + SRV t0x64 + UAV u0 + VB/IB)",
		"rootSig", fmt.Sprintf("0x%X", rootSig), "pso", fmt.Sprintf("0x%X", pso))
}

func (h *HostD3D12) initComputePipeline() {
	if h.Device == 0 || procD3D12SerializeRootSig.Find() != nil {
		return
	}
	// Compute RootSignature : Param 0 = Root CBV (b0), Param 1 = Root UAV (u0)
	params := [2]d3d12RootParameter{
		{ParameterType: 2 /* CBV */, Union0: 0, ShaderVisibility: 0},
		{ParameterType: 4 /* UAV */, Union0: 0, ShaderVisibility: 0},
	}
	rsDesc := d3d12RootSignatureDesc{
		NumParameters: 2,
		Parameters:    uintptr(unsafe.Pointer(&params[0])),
	}
	var rsBlob, errBlob uintptr
	hr, _, _ := procD3D12SerializeRootSig.Call(uintptr(unsafe.Pointer(&rsDesc)), 1, uintptr(unsafe.Pointer(&rsBlob)), uintptr(unsafe.Pointer(&errBlob)))
	if errBlob != 0 {
		_ = comCall(errBlob, 2)
	}
	if int32(hr) < 0 || rsBlob == 0 {
		return
	}
	defer comCall(rsBlob, 2)
	var csRS uintptr
	if int32(comCall(h.Device, 16, 0, comCall(rsBlob, 3), comCall(rsBlob, 4), uintptr(unsafe.Pointer(&guidID3D12RootSignature[0])), uintptr(unsafe.Pointer(&csRS)))) >= 0 && csRS != 0 {
		h.ComputeRootSig = csRS
	}
	// Buffer UAV 1 MiB persistant en heap DEFAULT (D3D12_RESOURCE_FLAG_ALLOW_UNORDERED_ACCESS = 0x4)
	heapProps := d3d12HeapProperties{Type: 1 /* DEFAULT */, CreationNodeMask: 1, VisibleNodeMask: 1}
	bufDesc := d3d12ResourceDesc{
		Dimension:        1, // BUFFER
		Width:            1 << 20,
		Height:           1,
		DepthOrArraySize: 1,
		MipLevels:        1,
		SampleCount:      1,
		Layout:           1, // ROW_MAJOR
		Flags:            0x4,
	}
	var uavRes uintptr
	if int32(comCall(h.Device, 27,
		uintptr(unsafe.Pointer(&heapProps)), 0,
		uintptr(unsafe.Pointer(&bufDesc)),
		0x8, // D3D12_RESOURCE_STATE_UNORDERED_ACCESS
		0,
		uintptr(unsafe.Pointer(&guidID3D12Resource[0])),
		uintptr(unsafe.Pointer(&uavRes)),
	)) >= 0 && uavRes != 0 {
		h.ComputeUAVBuf = uavRes
		h.ComputeUAVGPU = uint64(comCall(uavRes, 11)) // GetGPUVirtualAddress
	}
}

func fastPixelHash(b []byte) uint64 {
	var h uint64 = 0xcbf29ce484222325
	n := len(b)
	i := 0
	for ; i+8 <= n; i += 8 {
		v := *(*uint64)(unsafe.Pointer(&b[i]))
		h ^= v
		h *= 0x100000001b3
	}
	for ; i < n; i++ {
		h ^= uint64(b[i])
		h *= 0x100000001b3
	}
	return h ^ uint64(n)
}

func (h *HostD3D12) uploadTexturePixels(res uintptr, tx *HostTextureUpload, fmtID, bpe, block uint32) bool {
	if len(tx.Pixels) == 0 || h.UploadTex == 0 || h.UploadTexPtr == 0 {
		return false
	}
	bw := (tx.Width + block - 1) / block
	bh := (tx.Height + block - 1) / block
	srcRow := bw * bpe
	rowPitch := (srcRow + 255) &^ 255
	needBytes := uint64(rowPitch) * uint64(bh)
	const stagingCap = uint64(32 << 20)
	off := (h.texUploadOff + 511) &^ 511
	if needBytes == 0 || off+needBytes > stagingCap {
		return false
	}
	linear := DetileRDNA2(tx.Pixels, tx.Width, tx.Height, bpe, block, tx.Swizzle)
	if len(linear) == 0 {
		return false
	}
	dstSlice := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&h.UploadTexPtr))), stagingCap)[off : off+needBytes]
	for y := uint32(0); y < bh; y++ {
		sOff := uintptr(y) * uintptr(srcRow)
		dOff := uintptr(y) * uintptr(rowPitch)
		if sOff+uintptr(srcRow) <= uintptr(len(linear)) && dOff+uintptr(srcRow) <= uintptr(len(dstSlice)) {
			copy(dstSlice[dOff:dOff+uintptr(srcRow)], linear[sOff:sOff+uintptr(srcRow)])
		}
	}
	dstLoc := d3d12TextureCopyLocation{Resource: res, Type: 0 /* SUBRESOURCE_INDEX */}
	srcLoc := d3d12TextureCopyLocation{
		Resource: h.UploadTex,
		Type:     1, // PLACED_FOOTPRINT
		Offset:   off,
		Format:   fmtID,
		Width:    tx.Width,
		Height:   tx.Height,
		Depth:    1,
		RowPitch: rowPitch,
	}
	_ = comCall(h.CmdList, 16, uintptr(unsafe.Pointer(&dstLoc)), 0, 0, 0, uintptr(unsafe.Pointer(&srcLoc)), 0)
	h.texUploadOff = off + needBytes
	return true
}

// ensureHostTexture crée (ou retrouve et rafraîchit si modifiée) une Texture2D hôte + son SRV.
func (h *HostD3D12) ensureHostTexture(tx *HostTextureUpload) uintptr {
	if tx == nil || tx.GuestVA == 0 || tx.Width == 0 || tx.Height == 0 || tx.Width > 4096 || tx.Height > 4096 || h.SRVHeap == 0 || h.UploadTex == 0 || h.UploadTexPtr == 0 {
		return h.SRVGPUStart
	}
	bpe, block, fmtID := DXGIFormatMetrics(tx.Format)
	pixHash := fastPixelHash(tx.Pixels)
	if e, ok := h.TexCache[tx.GuestVA]; ok && e.width == tx.Width && e.height == tx.Height {
		// Rafraîchit automatiquement les textures dynamiques (ex: atlas de glyphes Scaleform 2048x2048 R8)
		if len(tx.Pixels) > 0 && e.hash != pixHash {
			bToCopy := d3d12ResourceBarrier{Resource: e.res, Subresource: 0xFFFFFFFF, StateBefore: 0xC0, StateAfter: 0x400}
			_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&bToCopy)))
			if h.uploadTexturePixels(e.res, tx, fmtID, bpe, block) {
				e.hash = pixHash
			}
			bToSRV := d3d12ResourceBarrier{Resource: e.res, Subresource: 0xFFFFFFFF, StateBefore: 0x400, StateAfter: 0xC0}
			_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&bToSRV)))
		}
		return e.srvGPU
	}
	if h.NextSRVSlot >= 2048 {
		return h.SRVGPUStart
	}
	bw := (tx.Width + block - 1) / block
	bh := (tx.Height + block - 1) / block
	srcRow := bw * bpe
	rowPitch := (srcRow + 255) &^ 255
	needBytes := uint64(rowPitch) * uint64(bh)
	if needBytes == 0 || needBytes > (32<<20) {
		return h.SRVGPUStart
	}
	heapProps := d3d12HeapProperties{Type: 1 /* DEFAULT */, CreationNodeMask: 1, VisibleNodeMask: 1}
	texDesc := d3d12ResourceDesc{
		Dimension:        3, // TEXTURE2D
		Width:            uint64(tx.Width),
		Height:           tx.Height,
		DepthOrArraySize: 1,
		MipLevels:        1,
		Format:           fmtID,
		SampleCount:      1,
		Layout:           0, // UNKNOWN
	}
	var res uintptr
	if int32(comCall(h.Device, 27,
		uintptr(unsafe.Pointer(&heapProps)), 0,
		uintptr(unsafe.Pointer(&texDesc)),
		0x400, // D3D12_RESOURCE_STATE_COPY_DEST
		0,
		uintptr(unsafe.Pointer(&guidID3D12Resource[0])),
		uintptr(unsafe.Pointer(&res)),
	)) < 0 || res == 0 {
		return h.SRVGPUStart
	}

	// Détuilage RDNA2 -> linéaire puis copie dans le buffer staging UPLOAD + CopyTextureRegion
	_ = h.uploadTexturePixels(res, tx, fmtID, bpe, block)
	b := d3d12ResourceBarrier{Resource: res, Subresource: 0xFFFFFFFF, StateBefore: 0x400 /* COPY_DEST */, StateAfter: 0xC0 /* NON_PIXEL | PIXEL_SHADER_RESOURCE */}
	_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&b)))

	slot := h.NextSRVSlot
	h.NextSRVSlot++
	cpuHandle := h.SRVCPUStart + uintptr(slot)*h.SRVStride
	gpuHandle := h.SRVGPUStart + uintptr(slot)*h.SRVStride
	// Mapping des composantes adapté aux formats 1/2 canaux utilisés par l'UI Scaleform :
	// - R8_UNORM (61), R16_FLOAT (54), R16_UNORM (56) -> RRRR (0x1000)
	// - R8G8_UNORM (49) -> RRRG (0x1200)
	compMap := uint32(0x1688) // D3D12_DEFAULT_SHADER_4_COMPONENT_MAPPING (RGBA)
	switch fmtID {
	case 54, 56, 61: // R16_FLOAT / R16_UNORM / R8_UNORM
		compMap = 0x1000 // RRRR
	case 49: // R8G8_UNORM
		compMap = 0x1200 // RRRG
	}
	srvDesc := d3d12SRVDesc{
		Format:                  fmtID,
		ViewDimension:           4, // TEXTURE2D
		Shader4ComponentMapping: compMap,
		MipLevels:               1,
	}
	_ = comCall(h.Device, 18, res, uintptr(unsafe.Pointer(&srvDesc)), cpuHandle)
	h.TexCache[tx.GuestVA] = &hostTexEntry{
		res:      res,
		width:    tx.Width,
		height:   tx.Height,
		format:   fmtID,
		srvGPU:   gpuHandle,
		hash:     pixHash,
		uploaded: true,
	}
	slog.Info("Host D3D12 Texture2D SRV créée et uploadée", "va", fmt.Sprintf("0x%X", tx.GuestVA), "w", tx.Width, "h", tx.Height, "guestFmt", tx.Format, "fmt", fmtID, "swz", fmt.Sprintf("0x%X", tx.Swizzle), "slot", slot)
	return gpuHandle
}

// ensureHostRT crée (ou retrouve) une Texture2D hôte utilisable à la fois comme RenderTarget (RTV)
// et comme ShaderResourceView (SRV t0) pour les passes intermédiaires offscreen -> composite.
func (h *HostD3D12) ensureHostRT(guestVA uint64) *hostRTEntry {
	if guestVA == 0 || h.Device == 0 || h.RTVHeap == 0 || h.SRVHeap == 0 || h.Width == 0 || h.Height == 0 {
		return nil
	}
	if e, ok := h.RTCache[guestVA]; ok {
		return e
	}
	if h.NextRTVSlot >= 256 || h.NextSRVSlot >= 2048 {
		return nil
	}
	heapProps := d3d12HeapProperties{Type: 1 /* DEFAULT */, CreationNodeMask: 1, VisibleNodeMask: 1}
	texDesc := d3d12ResourceDesc{
		Dimension:        3, // TEXTURE2D
		Width:            uint64(h.Width),
		Height:           h.Height,
		DepthOrArraySize: 1,
		MipLevels:        1,
		Format:           28, // DXGI_FORMAT_R8G8B8A8_UNORM
		SampleCount:      1,
		Layout:           0,   // UNKNOWN
		Flags:            0x1, // D3D12_RESOURCE_FLAG_ALLOW_RENDER_TARGET
	}
	var res uintptr
	if int32(comCall(h.Device, 27,
		uintptr(unsafe.Pointer(&heapProps)), 0,
		uintptr(unsafe.Pointer(&texDesc)),
		0x4, // D3D12_RESOURCE_STATE_RENDER_TARGET
		0,
		uintptr(unsafe.Pointer(&guidID3D12Resource[0])),
		uintptr(unsafe.Pointer(&res)),
	)) < 0 || res == 0 {
		return nil
	}
	rtvSlot := h.NextRTVSlot
	h.NextRTVSlot++
	rtvCPU := h.RTVStart + uintptr(rtvSlot)*h.RTVStride
	_ = comCall(h.Device, 20, res, 0, rtvCPU) // CreateRenderTargetView

	srvSlot := h.NextSRVSlot
	h.NextSRVSlot++
	srvCPU := h.SRVCPUStart + uintptr(srvSlot)*h.SRVStride
	srvGPU := h.SRVGPUStart + uintptr(srvSlot)*h.SRVStride
	srvDesc := d3d12SRVDesc{
		Format:                  28,
		ViewDimension:           4,      // TEXTURE2D
		Shader4ComponentMapping: 0x1688, // DEFAULT
		MipLevels:               1,
	}
	_ = comCall(h.Device, 18, res, uintptr(unsafe.Pointer(&srvDesc)), srvCPU) // CreateShaderResourceView

	e := &hostRTEntry{
		res:    res,
		rtvCPU: rtvCPU,
		srvGPU: srvGPU,
		width:  h.Width,
		height: h.Height,
		state:  0x4, // RENDER_TARGET
	}
	h.RTCache[guestVA] = e
	slog.Info("Host D3D12 RenderTarget offscreen créé (RTV+SRV)", "guestVA", fmt.Sprintf("0x%X", guestVA), "w", h.Width, "h", h.Height, "rtvSlot", rtvSlot, "srvSlot", srvSlot)
	return e
}

func (h *HostD3D12) createUploadBuffer(size uint64) (res uintptr, cpuPtr uintptr, gpuVA uint64) {
	heapProps := d3d12HeapProperties{Type: 2 /* UPLOAD */, CreationNodeMask: 1, VisibleNodeMask: 1}
	bufDesc := d3d12ResourceDesc{
		Dimension:        1, // BUFFER
		Width:            size,
		Height:           1,
		DepthOrArraySize: 1,
		MipLevels:        1,
		SampleCount:      1,
		Layout:           1, // ROW_MAJOR
	}
	if int32(comCall(h.Device, 27,
		uintptr(unsafe.Pointer(&heapProps)), 0,
		uintptr(unsafe.Pointer(&bufDesc)),
		0x00000A83, // GENERIC_READ
		0,
		uintptr(unsafe.Pointer(&guidID3D12Resource[0])),
		uintptr(unsafe.Pointer(&res)),
	)) < 0 || res == 0 {
		return 0, 0, 0
	}
	if int32(comCall(res, 8, 0, 0, uintptr(unsafe.Pointer(&cpuPtr)))) < 0 || cpuPtr == 0 {
		return res, 0, 0
	}
	gpuVA = uint64(comCall(res, 11)) // GetGPUVirtualAddress
	return res, cpuPtr, gpuVA
}
