package gpu

import (
	"fmt"
	"runtime"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
)

// BackendCapabilities décrit les fonctionnalités exposées par le backend GPU actif.
type BackendCapabilities struct {
	Name             string
	ShaderTarget     string // "dxil" ou "spirv"
	NativeWave64     bool
	MeshShaders      bool
	XDXRBVH4Builder  bool
	SupportsWARP     bool
}

// Backend définit l'interface commune aux backends graphiques D3D12 et Vulkan.
type Backend interface {
	Capabilities() BackendCapabilities
	Init() error
	Shutdown() error
}

// D3D12Backend implémente l'approche VIctory (Windows natif D3D12 + shaders DXIL SM 6.6 + shadow memory).
type D3D12Backend struct {
	UseWARP bool
	Debug   bool
}

func (b *D3D12Backend) Capabilities() BackendCapabilities {
	return BackendCapabilities{
		Name:            "d3d12-host",
		ShaderTarget:    "dxil",
		NativeWave64:    false,
		MeshShaders:     true,
		XDXRBVH4Builder: false,
		SupportsWARP:    true,
	}
}

func (b *D3D12Backend) Init() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("le backend D3D12 requiert Windows (OS actuel : %s)", runtime.GOOS)
	}
	return nil
}

func (b *D3D12Backend) Shutdown() error { return nil }

// VulkanBackend implémente l'approche Leonida (Vulkan 1.2/1.3 bindless + SPIR-V + émulation wave64 + XDXR BVH4).
type VulkanBackend struct {
	EmulateWave64 bool
	UseKHRRayAS   bool
}

func (b *VulkanBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{
		Name:            "vulkan-bindless",
		ShaderTarget:    "spirv",
		NativeWave64:    !b.EmulateWave64,
		MeshShaders:     true,
		XDXRBVH4Builder: !b.UseKHRRayAS,
		SupportsWARP:    false,
	}
}

func (b *VulkanBackend) Init() error     { return nil }
func (b *VulkanBackend) Shutdown() error { return nil }

// SelectBackend choisit le backend adapté à la configuration utilisateur et à l'OS.
func SelectBackend(kind core.GPUBackend, wave64Emulate string) Backend {
	switch kind {
	case core.BackendVulkan:
		return &VulkanBackend{EmulateWave64: wave64Emulate != "off"}
	case core.BackendD3D12:
		return &D3D12Backend{}
	default:
		if runtime.GOOS == "windows" {
			return &D3D12Backend{}
		}
		return &VulkanBackend{EmulateWave64: wave64Emulate != "off"}
	}
}
