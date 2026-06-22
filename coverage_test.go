package systemdboot

import (
	"errors"
	"sort"
	"strings"
	"testing"

	uefi "github.com/go-filesystems/uefi"
)

// memFS2 is an in-memory FS (mirrors the one in systemd_boot_test.go) for
// exercising the base reader's error and edge branches directly.
type memFS2 map[string][]byte

func (m memFS2) ReadFile(p string) ([]byte, error) {
	if b, ok := m[p]; ok {
		return b, nil
	}
	return nil, errors.New("not found: " + p)
}

func (m memFS2) ListDir(dir string) ([]string, error) {
	if dir != "" && !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	var out []string
	for k := range m {
		if !strings.HasPrefix(k, dir) {
			continue
		}
		rest := strings.TrimPrefix(k, dir)
		if strings.Contains(rest, "/") {
			continue
		}
		out = append(out, rest)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, errors.New("no such dir: " + dir)
	}
	return out, nil
}

// ---------- base-reader error / edge branches ----------

func TestDefault_LoaderConfMissing(t *testing.T) {
	if _, err := Default(memFS2{}, "/boot"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDefault_NoDefaultNoEntries(t *testing.T) {
	// loader.conf with no default and an empty entries dir → All errors
	// (ListDir on an empty dir returns "no such dir").
	fs := memFS2{"/boot/loader/loader.conf": []byte("timeout 5\n")}
	if _, err := Default(fs, "/boot"); err == nil {
		t.Error("expected error when no entries exist")
	}
}

func TestResolveDefault_LiteralMissing(t *testing.T) {
	fs := memFS2{"/boot/loader/loader.conf": []byte("default ghost\n")}
	if _, err := Default(fs, "/boot"); err == nil {
		t.Error("expected error for missing literal default entry")
	}
}

func TestResolveDefault_WildcardNoMatch(t *testing.T) {
	fs := memFS2{
		"/boot/loader/loader.conf":              []byte("default ubuntu-*\n"),
		"/boot/loader/entries/nixos-gen-1.conf": []byte("title NixOS\nlinux /a\n"),
	}
	if _, err := Default(fs, "/boot"); err == nil {
		t.Error("expected no-match error for wildcard default")
	}
}

func TestAll_SkipsUnreadableAndNonConf(t *testing.T) {
	fs := memFS2{
		"/boot/loader/entries/good.conf":  []byte("title Good\nsort-key z\nlinux /g\n"),
		"/boot/loader/entries/readme.txt": []byte("not an entry"),
	}
	entries, err := All(fs, "/boot")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(entries) != 1 || entries[0].EntryID != "good" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestBlsLess_EmptySortKeyOrdering(t *testing.T) {
	// Ascending sort: an empty sort-key sorts LAST, so it is NOT "less" than
	// an entry that has one; conversely an entry with a sort-key IS "less".
	withKey := &Boot{EntryID: "a", SortKey: "nixos"}
	without := &Boot{EntryID: "b"}
	if blsLess(without, withKey) {
		t.Error("empty sort-key should NOT be 'less' (sorts last)")
	}
	if !blsLess(withKey, without) {
		t.Error("entry with sort-key should be 'less' than one without")
	}
	// Same sort-key, different version.
	v1 := &Boot{SortKey: "x", Version: "1"}
	v2 := &Boot{SortKey: "x", Version: "2"}
	if !blsLess(v1, v2) {
		t.Error("version 1 should be less than version 2")
	}
}

func TestSplitFirstWS_NoWhitespace(t *testing.T) {
	k, v, ok := splitFirstWS("singletoken")
	if ok || k != "singletoken" || v != "" {
		t.Errorf("splitFirstWS = (%q,%q,%v)", k, v, ok)
	}
}

// ---------- uefi mutator error paths ----------

// faultStore wraps a real VariableStore and forces Set to fail, so the
// SetBootNext / SetBootOrder wrapper error branches are exercised.
type faultStore struct {
	uefi.VariableStore
}

func (faultStore) Set(uefi.Variable) error { return errors.New("write protected") }

func TestSetBootNext_Error(t *testing.T) {
	store := mintVarstore(t)
	if err := SetBootNext(faultStore{store}, 1); err == nil {
		t.Error("expected SetBootNext error")
	}
}

func TestSetBootOrder_Error(t *testing.T) {
	store := mintVarstore(t)
	if err := SetBootOrder(faultStore{store}, []uint16{1}); err == nil {
		t.Error("expected SetBootOrder error")
	}
}

func TestMakeSystemdBootFirst_SetError(t *testing.T) {
	store := mintVarstore(t)
	if _, err := uefi.AddBootEntry(store, sdBootLoadOption("Linux Boot Manager", `\EFI\systemd\systemd-bootx64.efi`)); err != nil {
		t.Fatal(err)
	}
	// faultStore makes the reorder write fail; the read still works because
	// List/Get pass through to the embedded store.
	if _, err := MakeSystemdBootFirst(faultStore{store}); err == nil {
		t.Error("expected MakeSystemdBootFirst write error")
	}
}

// TestReadBootState_SystemdBootNotInOrder covers the branch that surfaces a
// systemd-boot entry which is present as Boot#### but absent from BootOrder.
func TestReadBootState_SystemdBootNotInOrder(t *testing.T) {
	store := mintVarstore(t)
	num, err := uefi.AddBootEntry(store, sdBootLoadOption("Linux Boot Manager", `\EFI\systemd\systemd-bootx64.efi`))
	if err != nil {
		t.Fatal(err)
	}
	// Remove it from BootOrder while leaving the Boot#### entry in place.
	if err := uefi.SetBootOrder(store, []uint16{}); err != nil {
		t.Fatal(err)
	}
	state, err := ReadBootState(store)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range state.SystemdBootSlots {
		if n == num {
			found = true
		}
	}
	if !found {
		t.Errorf("systemd-boot slot %04X not surfaced when absent from BootOrder", num)
	}
	if state.Entries[num].InBootOrder {
		t.Error("entry should report InBootOrder=false")
	}
}
