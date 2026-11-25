//go:build windows

package gpu

import (
	"fmt"
	"log/slog"
	"syscall"
	"unsafe"
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
	GuestVA uint64
	Width   uint32
	Height  uint32
	Format  uint32 // DXGI_FORMAT
	Pixels  []byte
}

// HostDrawPass décrit un appel DrawInstanced rejoué sur le ID3D12Device matériel hôte.
type HostDrawPass struct {
	VertexCount   uint32
	InstanceCount uint32
	StartVertex   uint32
	StartInstance uint32
	PassIndex     uint32
	FrameNumber   uint32
	HostPSO       uintptr
	VBData        []byte
	VBStride      uint32
	CBData        []byte
	SRVTex        *HostTextureUpload
}

type hostTexEntry struct {
	res      uintptr
	width    uint32
	height   uint32
	format   uint32
	srvGPU   uintptr
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
	var codeBlob, errBlob uintptr
	hr, _, _ := procD3DCompile.Call(
		uintptr(unsafe.Pointer(&srcBytes[0])),
		uintptr(len(srcBytes)),
		0, 0, 0,
		uintptr(unsafe.Pointer(&entryBytes[0])),
		uintptr(unsafe.Pointer(&targetBytes[0])),
		0, 0,
		uintptr(unsafe.Pointer(&codeBlob)),
		uintptr(unsafe.Pointer(&errBlob)),
	)
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
	SemanticName  string
	SemanticIndex uint32
	Format        uint32 // DXGI_FORMAT (ex. 2 = R32G32B32A32_FLOAT)
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

// CompileGraphicsPSO compile une paire HLSL (VS + PS) et crée un ID3D12PipelineState matériel sur h.Device.
func (h *HostD3D12) CompileGraphicsPSO(vsHLSL, vsEntry, psHLSL, psEntry string, inputElems []HostInputElement) (uintptr, error) {
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
	return h.createGraphicsPSOFromDXBC(vsBC, psBC, inputElems)
}

func (h *HostD3D12) createGraphicsPSOFromDXBC(vsBC, psBC []byte, inputElems []HostInputElement) (uintptr, error) {
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
			rawDescs[i] = d3d12InputElementDesc{
				SemanticName:      uintptr(unsafe.Pointer(&nb[0])),
				SemanticIndex:     el.SemanticIndex,
				Format:            fmtID,
				InputSlot:         0,
				AlignedByteOffset: 0xFFFFFFFF, // D3D12_APPEND_ALIGNED_ELEMENT
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
	psoDesc.BlendState.RenderTarget[0] = d3d12RenderTargetBlendDesc{
		SrcBlend:              2, // ONE
		DestBlend:             1, // ZERO
		BlendOp:               1, // ADD
		SrcBlendAlpha:         2, // ONE
		DestBlendAlpha:        1, // ZERO
		BlendOpAlpha:          1, // ADD
		RenderTargetWriteMask: 0x0F,
	}
	psoDesc.RTVFormats[0] = 28 // DXGI_FORMAT_R8G8B8A8_UNORM

	var pso uintptr
	// ID3D12Device::CreateGraphicsPipelineState = slot 10
	hr := comCall(h.Device, 10, uintptr(unsafe.Pointer(&psoDesc)), uintptr(unsafe.Pointer(&guidID3D12PipelineState[0])), uintptr(unsafe.Pointer(&pso)))
	_ = nameBufs
	if int32(hr) < 0 || pso == 0 {
		return 0, fmt.Errorf("CreateGraphicsPipelineState échoué (0x%08X)", uint32(hr))
	}
	return pso, nil
}

// CompileHostPSO compile une paire HLSL (VS+PS) en ID3D12PipelineState sur l'instance HostD3D12 globale.
func CompileHostPSO(vsHLSL, vsEntry, psHLSL, psEntry string, inputElems []HostInputElement) (uintptr, error) {
	hostMu.Lock()
	h := hostInstance
	hostMu.Unlock()
	if h == nil {
		return 0, fmt.Errorf("HostD3D12 inactif")
	}
	return h.CompileGraphicsPSO(vsHLSL, vsEntry, psHLSL, psEntry, inputElems)
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

	// Param 0 : Root CBV dans b0 (cbuffer RootData : register(b0) { uint4 RootConst[64]; })
	// Param 1 : DescriptorTable 1 SRV dans t0 (Texture2D<float4> Tex0)
	srvRange := d3d12DescriptorRange{
		RangeType:                         0, // SRV
		NumDescriptors:                    1,
		BaseShaderRegister:                0,
		RegisterSpace:                     0,
		OffsetInDescriptorsFromTableStart: 0,
	}
	params := [2]d3d12RootParameter{
		{
			ParameterType:    2, // D3D12_ROOT_PARAMETER_TYPE_CBV
			Union0:           0, // ShaderRegister=0, RegisterSpace=0
			Union1:           0,
			ShaderVisibility: 0, // ALL
		},
		{
			ParameterType:    0, // DESCRIPTOR_TABLE
			Union0:           1, // NumDescriptorRanges=1
			Union1:           uint64(uintptr(unsafe.Pointer(&srvRange))),
			ShaderVisibility: 0, // ALL
		},
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
		NumParameters:     2,
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

	// Heap SRV shader-visible (64 descripteurs : slot 0 = null-descriptor par défaut, slots 1..63 = textures 2D hôtes)
	srvHeapDesc := d3d12DescriptorHeapDesc{Type: 0 /* CBV_SRV_UAV */, NumDescriptors: 64, Flags: 1 /* SHADER_VISIBLE */}
	if int32(comCall(h.Device, 14, uintptr(unsafe.Pointer(&srvHeapDesc)), uintptr(unsafe.Pointer(&guidID3D12DescriptorHeap[0])), uintptr(unsafe.Pointer(&h.SRVHeap)))) >= 0 && h.SRVHeap != 0 {
		var cpuStart, gpuStart uintptr
		_ = comCall(h.SRVHeap, 9, uintptr(unsafe.Pointer(&cpuStart)))  // GetCPUDescriptorHandleForHeapStart
		_ = comCall(h.SRVHeap, 10, uintptr(unsafe.Pointer(&gpuStart))) // GetGPUDescriptorHandleForHeapStart
		h.SRVCPUStart = cpuStart
		h.SRVGPUStart = gpuStart
		h.SRVStride = comCall(h.Device, 15, 0 /* CBV_SRV_UAV */)
		h.NextSRVSlot = 1
		h.TexCache = make(map[uint64]*hostTexEntry)
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
	// Buffers dynamiques persistants UPLOAD pour VB (256 KiB), CB (64 KiB) et Staging Texture (16 MiB)
	h.UploadVB, h.UploadVBPtr, h.UploadVBGPU = h.createUploadBuffer(256 << 10)
	h.UploadCB, h.UploadCBPtr, h.UploadCBGPU = h.createUploadBuffer(64 << 10)
	h.UploadTex, h.UploadTexPtr, _ = h.createUploadBuffer(16 << 20)
	slog.Info("Host D3D12 Replay PSO matériel compilé (VS+PS DXBC, RootCBV b0 + SRV t0x64 + VB)",
		"rootSig", fmt.Sprintf("0x%X", rootSig), "pso", fmt.Sprintf("0x%X", pso))
}

// ensureHostTexture crée (ou retrouve) une Texture2D hôte + son SRV et planifie son upload si besoin.
func (h *HostD3D12) ensureHostTexture(tx *HostTextureUpload) uintptr {
	if tx == nil || tx.GuestVA == 0 || tx.Width == 0 || tx.Height == 0 || tx.Width > 4096 || tx.Height > 4096 || h.SRVHeap == 0 || h.UploadTex == 0 || h.UploadTexPtr == 0 {
		return h.SRVGPUStart
	}
	if e, ok := h.TexCache[tx.GuestVA]; ok && e.width == tx.Width && e.height == tx.Height {
		return e.srvGPU
	}
	if h.NextSRVSlot >= 64 {
		return h.SRVGPUStart
	}
	// Format RGBA8 par défaut si typeless/non standard
	fmtID := tx.Format
	if fmtID != 28 && fmtID != 29 && fmtID != 87 && fmtID != 91 {
		fmtID = 28 // DXGI_FORMAT_R8G8B8A8_UNORM
	}
	rowPitch := ((tx.Width * 4) + 255) &^ 255
	needBytes := uint64(rowPitch) * uint64(tx.Height)
	if needBytes > (16 << 20) {
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

	// Copie des pixels dans le buffer d'upload staging puis CopyTextureRegion -> PIXEL_SHADER_RESOURCE
	if len(tx.Pixels) > 0 {
		dstSlice := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&h.UploadTexPtr))), 16<<20)
		srcRow := tx.Width * 4
		for y := uint32(0); y < tx.Height; y++ {
			sOff := uintptr(y) * uintptr(srcRow)
			dOff := uintptr(y) * uintptr(rowPitch)
			if sOff+uintptr(srcRow) <= uintptr(len(tx.Pixels)) && dOff+uintptr(srcRow) <= uintptr(len(dstSlice)) {
				copy(dstSlice[dOff:dOff+uintptr(srcRow)], tx.Pixels[sOff:sOff+uintptr(srcRow)])
			}
		}
		dstLoc := d3d12TextureCopyLocation{Resource: res, Type: 0 /* SUBRESOURCE_INDEX */}
		srcLoc := d3d12TextureCopyLocation{
			Resource: h.UploadTex,
			Type:     1, // PLACED_FOOTPRINT
			Format:   fmtID,
			Width:    tx.Width,
			Height:   tx.Height,
			Depth:    1,
			RowPitch: rowPitch,
		}
		_ = comCall(h.CmdList, 16, uintptr(unsafe.Pointer(&dstLoc)), 0, 0, 0, uintptr(unsafe.Pointer(&srcLoc)), 0)
	}
	b := d3d12ResourceBarrier{Resource: res, Subresource: 0xFFFFFFFF, StateBefore: 0x400 /* COPY_DEST */, StateAfter: 0x80 /* PIXEL_SHADER_RESOURCE */}
	_ = comCall(h.CmdList, 26, 1, uintptr(unsafe.Pointer(&b)))

	slot := h.NextSRVSlot
	h.NextSRVSlot++
	cpuHandle := h.SRVCPUStart + uintptr(slot)*h.SRVStride
	gpuHandle := h.SRVGPUStart + uintptr(slot)*h.SRVStride
	srvDesc := d3d12SRVDesc{
		Format:                  fmtID,
		ViewDimension:           4,      // TEXTURE2D
		Shader4ComponentMapping: 0x1688, // DEFAULT
		MipLevels:               1,
	}
	_ = comCall(h.Device, 18, res, uintptr(unsafe.Pointer(&srvDesc)), cpuHandle)
	h.TexCache[tx.GuestVA] = &hostTexEntry{
		res:      res,
		width:    tx.Width,
		height:   tx.Height,
		format:   fmtID,
		srvGPU:   gpuHandle,
		uploaded: true,
	}
	slog.Info("Host D3D12 Texture2D SRV créée et uploadée", "va", fmt.Sprintf("0x%X", tx.GuestVA), "w", tx.Width, "h", tx.Height, "fmt", fmtID, "slot", slot)
	return gpuHandle
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
