//go:build windows

package gpu

import (
	"fmt"
	"log/slog"
	"syscall"
	"unsafe"
)

var (
	modD3DCompiler47             = syscall.NewLazyDLL("d3dcompiler_47.dll")
	procD3DCompile               = modD3DCompiler47.NewProc("D3DCompile")
	procD3D12SerializeRootSig    = modD3D12Host.NewProc("D3D12SerializeRootSignature")

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

// HostDrawPass décrit un appel DrawInstanced rejoué sur le ID3D12Device matériel hôte.
type HostDrawPass struct {
	VertexCount   uint32
	InstanceCount uint32
	StartVertex   uint32
	StartInstance uint32
	PassIndex     uint32
	FrameNumber   uint32
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

type d3d12RootConstants struct {
	ShaderRegister uint32
	RegisterSpace  uint32
	Num32BitValues uint32
}

type d3d12RootParameter struct {
	ParameterType    uint32
	_pad             uint32
	Constants        d3d12RootConstants
	_padUnion        uint32
	ShaderVisibility uint32
	_padTail         uint32
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

// initReplayPipeline compile le pipeline plein écran matériel hôte (RootSignature + PSO)
// permettant de rejouer les appels DrawInstanced(3, 1) directement sur le GPU Windows.
func (h *HostD3D12) initReplayPipeline() {
	if h.Device == 0 {
		return
	}
	if err := procD3D12SerializeRootSig.Find(); err != nil {
		return
	}

	// RootSignature avec 4 constantes 32-bit dans b0 (frame, passIndex, drawCount, reserved)
	param := d3d12RootParameter{
		ParameterType:    1, // D3D12_ROOT_PARAMETER_TYPE_32BIT_CONSTANTS
		Constants:        d3d12RootConstants{ShaderRegister: 0, RegisterSpace: 0, Num32BitValues: 4},
		ShaderVisibility: 0, // ALL
	}
	rsDesc := d3d12RootSignatureDesc{
		NumParameters: 1,
		Parameters:    uintptr(unsafe.Pointer(&param)),
		Flags:         0,
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
	// ID3D12Device::CreateRootSignature = slot 16
	hr = comCall(h.Device, 16, 0, rsPtr, rsSize, uintptr(unsafe.Pointer(&guidID3D12RootSignature[0])), uintptr(unsafe.Pointer(&rootSig)))
	if int32(hr) < 0 || rootSig == 0 {
		slog.Warn("Host D3D12 CreateRootSignature échoué", "hr", fmt.Sprintf("0x%08X", uint32(hr)))
		return
	}

	const hlslSrc = `
cbuffer PassData : register(b0) {
    uint FrameNum;
    uint PassIndex;
    uint TotalPasses;
    uint Pad0;
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
    float t = float(FrameNum) * 0.03;
    float pulse = 0.08 + 0.04 * sin(t + float(PassIndex) * 1.1);
    float3 base = float3(0.04 + pulse * 0.4, 0.05 + pulse * 0.5, 0.10 + pulse);
    float vign = 1.0 - 0.35 * dot(i.uv - 0.5, i.uv - 0.5);
    return float4(base * vign, 1.0);
}
`
	vsBC, err := CompileHLSL(hlslSrc, "VSMain", "vs_5_0")
	if err != nil {
		slog.Warn("Host D3D12 CompileHLSL VS échoué", "err", err)
		return
	}
	psBC, err := CompileHLSL(hlslSrc, "PSMain", "ps_5_0")
	if err != nil {
		slog.Warn("Host D3D12 CompileHLSL PS échoué", "err", err)
		return
	}

	psoDesc := d3d12GraphicsPSODesc{
		RootSignature: rootSig,
		VS:            d3d12ShaderBytecode{Ptr: uintptr(unsafe.Pointer(&vsBC[0])), Len: uintptr(len(vsBC))},
		PS:            d3d12ShaderBytecode{Ptr: uintptr(unsafe.Pointer(&psBC[0])), Len: uintptr(len(psBC))},
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
	hr = comCall(h.Device, 10, uintptr(unsafe.Pointer(&psoDesc)), uintptr(unsafe.Pointer(&guidID3D12PipelineState[0])), uintptr(unsafe.Pointer(&pso)))
	if int32(hr) < 0 || pso == 0 {
		slog.Warn("Host D3D12 CreateGraphicsPipelineState échoué", "hr", fmt.Sprintf("0x%08X", uint32(hr)))
		return
	}

	h.ReplayRootSig = rootSig
	h.ReplayPSO = pso
	slog.Info("Host D3D12 Replay PSO matériel compilé (VS+PS DXBC)", "rootSig", fmt.Sprintf("0x%X", rootSig), "pso", fmt.Sprintf("0x%X", pso))
}
