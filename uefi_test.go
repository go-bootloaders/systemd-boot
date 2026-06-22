package systemdboot

import (
	"path/filepath"
	"testing"

	uefi "github.com/go-filesystems/uefi"
)

// mintVarstore formats a fresh non-auth UEFI variable store and returns an
// opened Store. The caller must Close it.
func mintVarstore(t *testing.T) uefi.Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vars.fd")
	store, err := uefi.Format(p, uefi.VarsSizeX86_64)
	if err != nil {
		t.Fatalf("uefi.Format: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// sdBootLoadOption builds a LoadOption pointing at systemd-boot's EFI binary,
// behind a HardDrive device-path node so the rendered text is realistic.
func sdBootLoadOption(desc, efiPath string) *uefi.LoadOption {
	hd := uefi.HardDriveNode{
		PartitionNumber: 1,
		PartitionStart:  2048,
		PartitionSize:   0x20000,
		MBRType:         2, // GPT
		SignatureType:   2,
	}
	return &uefi.LoadOption{
		Attributes:  uefi.LoadOptionActive,
		Description: desc,
		DevicePath: []uefi.DevicePathNode{
			hd.Node(),
			uefi.FilePathNode(efiPath),
		},
	}
}

func TestReadBootState_RoundTrip(t *testing.T) {
	store := mintVarstore(t)

	// Boot0000: systemd-boot. Boot0001: some other OS loader.
	sdNum, err := uefi.AddBootEntry(store, sdBootLoadOption("Linux Boot Manager", `\EFI\systemd\systemd-bootx64.efi`))
	if err != nil {
		t.Fatalf("AddBootEntry sd-boot: %v", err)
	}
	otherNum, err := uefi.AddBootEntry(store, sdBootLoadOption("Some OS", `\EFI\other\grubx64.efi`))
	if err != nil {
		t.Fatalf("AddBootEntry other: %v", err)
	}

	state, err := ReadBootState(store)
	if err != nil {
		t.Fatalf("ReadBootState: %v", err)
	}

	if len(state.Order) < 2 {
		t.Fatalf("BootOrder too short: %v", state.Order)
	}
	sd := state.Entries[sdNum]
	if sd == nil {
		t.Fatalf("sd-boot entry %04X missing", sdNum)
	}
	if !sd.IsSystemdBoot {
		t.Errorf("sd-boot entry not flagged IsSystemdBoot (path text %q)", sd.DevicePathText)
	}
	if !sd.Active || !sd.InBootOrder {
		t.Errorf("sd-boot entry Active=%v InBootOrder=%v", sd.Active, sd.InBootOrder)
	}
	if sd.Description != "Linux Boot Manager" {
		t.Errorf("description = %q", sd.Description)
	}
	if other := state.Entries[otherNum]; other == nil || other.IsSystemdBoot {
		t.Errorf("other entry wrongly flagged as systemd-boot: %+v", other)
	}
	if len(state.SystemdBootSlots) != 1 || state.SystemdBootSlots[0] != sdNum {
		t.Errorf("SystemdBootSlots = %v, want [%04X]", state.SystemdBootSlots, sdNum)
	}
	if state.BootNextSet {
		t.Errorf("BootNext unexpectedly set")
	}
}

func TestSetBootNext_RoundTrip(t *testing.T) {
	store := mintVarstore(t)
	sdNum, err := uefi.AddBootEntry(store, sdBootLoadOption("Linux Boot Manager", `\EFI\systemd\systemd-bootx64.efi`))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetBootNext(store, sdNum); err != nil {
		t.Fatalf("SetBootNext: %v", err)
	}
	state, err := ReadBootState(store)
	if err != nil {
		t.Fatal(err)
	}
	if !state.BootNextSet || state.BootNext != sdNum {
		t.Errorf("BootNext = %04X (set=%v), want %04X", state.BootNext, state.BootNextSet, sdNum)
	}
}

func TestMakeSystemdBootFirst(t *testing.T) {
	store := mintVarstore(t)
	// Add a non-sd-boot entry first so it occupies BootOrder[0].
	if _, err := uefi.AddBootEntry(store, sdBootLoadOption("Some OS", `\EFI\other\grubx64.efi`)); err != nil {
		t.Fatal(err)
	}
	sdNum, err := uefi.AddBootEntry(store, sdBootLoadOption("Linux Boot Manager", `\EFI\systemd\systemd-bootx64.efi`))
	if err != nil {
		t.Fatal(err)
	}

	target, err := MakeSystemdBootFirst(store)
	if err != nil {
		t.Fatalf("MakeSystemdBootFirst: %v", err)
	}
	if target != sdNum {
		t.Errorf("target = %04X, want %04X", target, sdNum)
	}
	order, err := uefi.BootOrder(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) == 0 || order[0] != sdNum {
		t.Errorf("BootOrder[0] = %v, want first = %04X (full %v)", order, sdNum, order)
	}
}

func TestMakeSystemdBootFirst_NoSdBoot(t *testing.T) {
	store := mintVarstore(t)
	if _, err := uefi.AddBootEntry(store, sdBootLoadOption("Some OS", `\EFI\other\grubx64.efi`)); err != nil {
		t.Fatal(err)
	}
	if _, err := MakeSystemdBootFirst(store); err == nil {
		t.Error("expected error when no entry points at systemd-boot")
	}
}

func TestSetBootOrder(t *testing.T) {
	store := mintVarstore(t)
	a, _ := uefi.AddBootEntry(store, sdBootLoadOption("A", `\EFI\a\a.efi`))
	b, _ := uefi.AddBootEntry(store, sdBootLoadOption("B", `\EFI\b\b.efi`))
	if err := SetBootOrder(store, []uint16{b, a}); err != nil {
		t.Fatalf("SetBootOrder: %v", err)
	}
	order, err := uefi.BootOrder(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != b || order[1] != a {
		t.Errorf("BootOrder = %v, want [%04X %04X]", order, b, a)
	}
}

// detectFallbackPath verifies the removable-media fallback path is also
// recognised as systemd-boot.
func TestDevicePathIsSystemdBoot_Fallback(t *testing.T) {
	store := mintVarstore(t)
	n, err := uefi.AddBootEntry(store, sdBootLoadOption("Fallback", `\EFI\BOOT\BOOTX64.EFI`))
	if err != nil {
		t.Fatal(err)
	}
	state, err := ReadBootState(store)
	if err != nil {
		t.Fatal(err)
	}
	if be := state.Entries[n]; be == nil || !be.IsSystemdBoot {
		t.Errorf("fallback path not recognised as systemd-boot: %+v", be)
	}
}
