// uefi.go integrates the UEFI boot-variable namespace
// (go-filesystems/uefi) alongside the BLS reader. systemd-boot installs
// itself as a UEFI Boot#### LoadOption pointing at its EFI binary; the BLS
// entries it then presents (loader/entries/*.conf) are a separate, second
// stage. A complete picture of a systemd-boot install therefore needs both:
// the firmware BootOrder/Boot#### state AND the BLS entries.
//
// This file exposes the boot-variable state (list BootOrder + Boot####
// LoadOptions with device-path text), cross-references which Boot#### points
// at the systemd-boot binary, and provides BootNext / BootOrder mutators.

package systemdboot

import (
	"fmt"
	"strings"

	uefi "github.com/go-filesystems/uefi"
)

// Canonical ESP-relative paths at which systemd-boot installs its EFI
// executable. The first is its own vendor path; the second is the removable-
// media fallback (\EFI\BOOT\BOOTX64.EFI) it also populates so firmware that
// only honours the fallback path still boots it. Matching is case-insensitive
// because UEFI FAT paths are case-insensitive.
var systemdBootEFIPaths = []string{
	`\EFI\systemd\systemd-bootx64.efi`,
	`\EFI\systemd\systemd-bootaa64.efi`,
	`\EFI\BOOT\BOOTX64.EFI`,
	`\EFI\BOOT\BOOTAA64.EFI`,
}

// BootEntry is a UEFI Boot#### LoadOption lifted into a friendly shape, with
// the device path already rendered to text and the systemd-boot match
// pre-computed.
type BootEntry struct {
	// Number is the Boot#### index (e.g. 0x0001 for Boot0001).
	Number uint16

	// Description is the human-readable label (UEFI "Description" field).
	Description string

	// DevicePathText is the device path rendered to the standard UEFI text
	// representation, e.g.
	// "HD(1,GPT,<guid>,…)/File(\EFI\systemd\systemd-bootx64.efi)".
	DevicePathText string

	// Active reflects the LOAD_OPTION_ACTIVE attribute bit.
	Active bool

	// InBootOrder is true if this entry's number appears in BootOrder.
	InBootOrder bool

	// IsSystemdBoot is true if the device path's File() segment matches one
	// of systemd-boot's canonical EFI paths.
	IsSystemdBoot bool
}

// BootState is the firmware boot-variable picture: the ordered BootOrder, the
// resolved Boot#### entries (keyed by number), BootNext if set, and the
// numbers (in BootOrder sequence) that point at systemd-boot.
type BootState struct {
	Order            []uint16
	Entries          map[uint16]*BootEntry
	BootNext         uint16
	BootNextSet      bool
	SystemdBootSlots []uint16
}

// ReadBootState reads the full UEFI boot-variable state from store (a varstore
// opened/minted via go-filesystems/uefi). It lists every Boot#### LoadOption,
// renders device paths to text, cross-references BootOrder and BootNext, and
// flags entries that point at systemd-boot.
func ReadBootState(store uefi.VariableStore) (*BootState, error) {
	options, _, err := uefi.ListBootEntries(store)
	if err != nil {
		return nil, fmt.Errorf("systemd-boot: list boot entries: %w", err)
	}

	// Use the RAW BootOrder for membership and ordering. ListBootEntries also
	// appends not-in-order entries to its returned slice, which would wrongly
	// mark them as InBootOrder; the firmware's BootOrder is the truth.
	order, err := uefi.BootOrder(store)
	if err != nil {
		return nil, fmt.Errorf("systemd-boot: read BootOrder: %w", err)
	}

	inOrder := make(map[uint16]bool, len(order))
	for _, n := range order {
		inOrder[n] = true
	}

	entries := make(map[uint16]*BootEntry, len(options))
	for num, lo := range options {
		be := loadOptionToBootEntry(num, lo, inOrder[num])
		entries[num] = be
	}

	state := &BootState{
		Order:   order,
		Entries: entries,
	}

	// systemd-boot slots, reported in BootOrder sequence so the caller sees
	// them in firmware-evaluation order.
	for _, n := range order {
		if be, ok := entries[n]; ok && be.IsSystemdBoot {
			state.SystemdBootSlots = append(state.SystemdBootSlots, n)
		}
	}
	// Entries that point at systemd-boot but are not in BootOrder are still
	// worth surfacing, appended after the ordered ones.
	for num, be := range entries {
		if be.IsSystemdBoot && !inOrder[num] {
			state.SystemdBootSlots = append(state.SystemdBootSlots, num)
		}
	}

	if next, ok, err := uefi.BootNext(store); err != nil {
		return nil, fmt.Errorf("systemd-boot: read BootNext: %w", err)
	} else if ok {
		state.BootNext = next
		state.BootNextSet = true
	}

	return state, nil
}

// loadOptionToBootEntry lifts a uefi.LoadOption into a BootEntry.
func loadOptionToBootEntry(num uint16, lo *uefi.LoadOption, inOrder bool) *BootEntry {
	text := uefi.DevicePathText(lo.DevicePath)
	return &BootEntry{
		Number:         num,
		Description:    lo.Description,
		DevicePathText: text,
		Active:         lo.Attributes&uefi.LoadOptionActive != 0,
		InBootOrder:    inOrder,
		IsSystemdBoot:  devicePathIsSystemdBoot(lo),
	}
}

// devicePathIsSystemdBoot reports whether a LoadOption's device path ends in a
// File() node naming one of systemd-boot's canonical EFI executables. The
// comparison is case-insensitive (UEFI FAT paths are).
func devicePathIsSystemdBoot(lo *uefi.LoadOption) bool {
	for _, node := range lo.DevicePath {
		fp, ok := node.FilePath()
		if !ok {
			continue
		}
		for _, want := range systemdBootEFIPaths {
			if strings.EqualFold(fp, want) {
				return true
			}
		}
	}
	return false
}

// SetBootNext sets the one-shot UEFI BootNext variable to n (the next boot,
// and only the next boot, will use Boot####=n). Thin pass-through to
// go-filesystems/uefi so callers don't import it directly for the common case.
func SetBootNext(store uefi.VariableStore, n uint16) error {
	if err := uefi.SetBootNext(store, n); err != nil {
		return fmt.Errorf("systemd-boot: set BootNext=%04X: %w", n, err)
	}
	return nil
}

// SetBootOrder replaces the UEFI BootOrder with the given sequence.
func SetBootOrder(store uefi.VariableStore, order []uint16) error {
	if err := uefi.SetBootOrder(store, order); err != nil {
		return fmt.Errorf("systemd-boot: set BootOrder: %w", err)
	}
	return nil
}

// MakeSystemdBootFirst rewrites BootOrder so the first Boot#### that points at
// systemd-boot is evaluated first, preserving the relative order of the rest.
// Returns the chosen systemd-boot slot. Returns an error if no Boot#### points
// at systemd-boot.
func MakeSystemdBootFirst(store uefi.VariableStore) (uint16, error) {
	state, err := ReadBootState(store)
	if err != nil {
		return 0, err
	}
	if len(state.SystemdBootSlots) == 0 {
		return 0, fmt.Errorf("systemd-boot: no Boot#### entry points at systemd-boot")
	}
	target := state.SystemdBootSlots[0]

	newOrder := make([]uint16, 0, len(state.Order)+1)
	newOrder = append(newOrder, target)
	for _, n := range state.Order {
		if n != target {
			newOrder = append(newOrder, n)
		}
	}
	// If target wasn't in the existing order, it's now prepended; if it was,
	// it's been moved to the front. Either way write it back.
	if err := SetBootOrder(store, newOrder); err != nil {
		return 0, err
	}
	return target, nil
}
