//go:build windows

package hle

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestXGameSavePersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	gs := &gameSaveState{
		rootDir:     dir,
		containers:  make(map[uintptr]*gsContainerHandle),
		updates:     make(map[uintptr]*gsUpdateHandle),
		asyncReads:  make(map[uintptr]*gsAsyncReadResult),
		asyncStatus: make(map[uintptr]uint32),
	}

	cDir := gsContainerDir(dir, "profile/settings")
	u := &gsUpdateHandle{
		name:        "profile/settings",
		dir:         cDir,
		displayName: "Profile Settings",
		changes: []gsBlobChange{
			{name: "eula_accepted", data: []byte{1, 2, 3, 4}},
			{name: "gamma", data: []byte("0.55")},
		},
	}
	if hr := gs.applyUpdateLocked(u); hr != uint32(hresultOK) {
		t.Fatalf("applyUpdateLocked failed: 0x%X", hr)
	}

	// Re-enumerate containers from disk
	list := gs.listContainersLocked(nil)
	if len(list) != 1 || list[0].meta.Name != "profile/settings" || len(list[0].meta.Blobs) != 2 {
		t.Fatalf("unexpected container list: %+v", list)
	}
	if list[0].total != 8 {
		t.Fatalf("expected total 8 bytes, got %d", list[0].total)
	}

	// Read blobs back and verify packGameSaveBlobs layout
	blobs, hr := gs.readBlobsLocked(&gsContainerHandle{name: "profile/settings", dir: cDir}, []string{"eula_accepted", "gamma"})
	if hr != uint32(hresultOK) || len(blobs) != 2 {
		t.Fatalf("readBlobsLocked failed: hr=0x%X len=%d", hr, len(blobs))
	}
	if !bytes.Equal(blobs[0].data, []byte{1, 2, 3, 4}) || string(blobs[1].data) != "0.55" {
		t.Fatalf("blob mismatch: %+v", blobs)
	}

	const base = uintptr(0x100000)
	packed := packGameSaveBlobs(blobs, base)
	name0Off := binary.LittleEndian.Uint64(packed[0:8]) - uint64(base)
	size0 := binary.LittleEndian.Uint32(packed[8:12])
	data0Off := binary.LittleEndian.Uint64(packed[16:24]) - uint64(base)
	if size0 != 4 || string(packed[name0Off:name0Off+13]) != "eula_accepted" || !bytes.Equal(packed[data0Off:data0Off+4], []byte{1, 2, 3, 4}) {
		t.Fatalf("packGameSaveBlobs entry 0 invalid: nameOff=%d size=%d dataOff=%d", name0Off, size0, data0Off)
	}

	// Delete one blob and verify
	uDel := &gsUpdateHandle{
		name:    "profile/settings",
		dir:     cDir,
		changes: []gsBlobChange{{name: "gamma", delete: true}},
	}
	if hr := gs.applyUpdateLocked(uDel); hr != uint32(hresultOK) {
		t.Fatalf("delete update failed: 0x%X", hr)
	}
	if _, hr := gs.readBlobsLocked(&gsContainerHandle{name: "profile/settings", dir: cDir}, []string{"gamma"}); hr != hresultGSBlobNotFound {
		t.Fatalf("expected BLOB_NOT_FOUND after delete, got 0x%X", hr)
	}
}
