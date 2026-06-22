package systemdboot

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

// memFS is a tiny in-memory FS for tests. Keys are absolute paths;
// directory listing is computed from the prefix.
type memFS map[string][]byte

func (m memFS) ReadFile(p string) ([]byte, error) {
	if b, ok := m[p]; ok {
		return b, nil
	}
	return nil, errors.New("not found: " + p)
}

func (m memFS) ListDir(dir string) ([]string, error) {
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

// ---------- parseKV / parseEntry ----------

func TestParseKV_Basic(t *testing.T) {
	const src = `
# A comment
default     nixos-generation-42.conf
timeout     5

# trailing comment
editor   no
`
	pairs, err := parseKV(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if pairs["default"] != "nixos-generation-42.conf" {
		t.Errorf("default = %q", pairs["default"])
	}
	if pairs["timeout"] != "5" {
		t.Errorf("timeout = %q", pairs["timeout"])
	}
	if pairs["editor"] != "no" {
		t.Errorf("editor = %q", pairs["editor"])
	}
}

func TestParseEntry_MultipleOptions(t *testing.T) {
	const src = `
title    NixOS
linux    /EFI/nixos/abc-linux.efi
initrd   /EFI/nixos/abc-initrd
options  init=/nix/store/x-init root=/dev/disk/by-label/nixos
options  systemd.show_status=true
`
	pairs, opts, err := parseEntry(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if pairs["title"] != "NixOS" {
		t.Errorf("title = %q", pairs["title"])
	}
	if pairs["linux"] != "/EFI/nixos/abc-linux.efi" {
		t.Errorf("linux = %q", pairs["linux"])
	}
	want := "init=/nix/store/x-init root=/dev/disk/by-label/nixos systemd.show_status=true"
	if opts != want {
		t.Errorf("options = %q, want %q", opts, want)
	}
}

// ---------- LoadLoaderConf ----------

func TestLoadLoaderConf(t *testing.T) {
	fs := memFS{
		"/boot/loader/loader.conf": []byte("default nixos-generation-42.conf\ntimeout 5\n"),
	}
	lc, err := LoadLoaderConf(fs, "/boot")
	if err != nil {
		t.Fatal(err)
	}
	if lc.Default != "nixos-generation-42" {
		t.Errorf("default = %q, want %q (with .conf stripped)", lc.Default, "nixos-generation-42")
	}
	if lc.Timeout != "5" {
		t.Errorf("timeout = %q", lc.Timeout)
	}
}

func TestLoadLoaderConf_Missing(t *testing.T) {
	_, err := LoadLoaderConf(memFS{}, "/boot")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// ---------- Default + All (NixOS-style fixture) ----------

var nixosFixture = memFS{
	"/boot/loader/loader.conf": []byte(`default nixos-generation-42.conf
timeout 5
`),
	"/boot/loader/entries/nixos-generation-40.conf": []byte(`title    NixOS
version  Generation 40 NixOS 23.05
sort-key nixos
linux    /EFI/nixos/aaa-linux.efi
initrd   /EFI/nixos/aaa-initrd
options  init=/nix/store/aaa-init root=/dev/disk/by-label/nixos
`),
	"/boot/loader/entries/nixos-generation-41.conf": []byte(`title    NixOS
version  Generation 41 NixOS 23.11
sort-key nixos
linux    /EFI/nixos/bbb-linux.efi
initrd   /EFI/nixos/bbb-initrd
options  init=/nix/store/bbb-init root=/dev/disk/by-label/nixos
`),
	"/boot/loader/entries/nixos-generation-42.conf": []byte(`title    NixOS
version  Generation 42 NixOS 24.05
sort-key nixos
linux    /EFI/nixos/ccc-linux.efi
initrd   /EFI/nixos/ccc-initrd
options  init=/nix/store/ccc-init root=/dev/disk/by-label/nixos
`),
}

func TestDefault_NixOSExplicitGeneration(t *testing.T) {
	got, err := Default(nixosFixture, "/boot")
	if err != nil {
		t.Fatal(err)
	}
	if got.EntryID != "nixos-generation-42" {
		t.Errorf("EntryID = %q, want %q", got.EntryID, "nixos-generation-42")
	}
	if got.LinuxPath != "/EFI/nixos/ccc-linux.efi" {
		t.Errorf("LinuxPath = %q", got.LinuxPath)
	}
	if got.InitrdPath != "/EFI/nixos/ccc-initrd" {
		t.Errorf("InitrdPath = %q", got.InitrdPath)
	}
	want := "init=/nix/store/ccc-init root=/dev/disk/by-label/nixos"
	if got.Options != want {
		t.Errorf("Options = %q, want %q", got.Options, want)
	}
}

func TestDefault_NixOSWildcard(t *testing.T) {
	wildcard := memFS{
		"/boot/loader/loader.conf":                      []byte("default nixos-generation-*.conf\n"),
		"/boot/loader/entries/nixos-generation-40.conf": nixosFixture["/boot/loader/entries/nixos-generation-40.conf"],
		"/boot/loader/entries/nixos-generation-41.conf": nixosFixture["/boot/loader/entries/nixos-generation-41.conf"],
		"/boot/loader/entries/nixos-generation-42.conf": nixosFixture["/boot/loader/entries/nixos-generation-42.conf"],
	}
	got, err := Default(wildcard, "/boot")
	if err != nil {
		t.Fatal(err)
	}
	if got.EntryID != "nixos-generation-42" {
		t.Errorf("wildcard picked %q, want nixos-generation-42 (newest)", got.EntryID)
	}
}

func TestDefault_NoDefaultClause(t *testing.T) {
	// loader.conf with no `default` should pick the BLS-highest entry.
	fs := memFS{
		"/boot/loader/loader.conf":                      []byte("timeout 5\n"),
		"/boot/loader/entries/nixos-generation-40.conf": nixosFixture["/boot/loader/entries/nixos-generation-40.conf"],
		"/boot/loader/entries/nixos-generation-42.conf": nixosFixture["/boot/loader/entries/nixos-generation-42.conf"],
	}
	got, err := Default(fs, "/boot")
	if err != nil {
		t.Fatal(err)
	}
	if got.EntryID != "nixos-generation-42" {
		t.Errorf("no-default picked %q, want nixos-generation-42", got.EntryID)
	}
}

func TestAll_SortOrder(t *testing.T) {
	got, err := All(nixosFixture, "/boot")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	wantOrder := []string{"nixos-generation-42", "nixos-generation-41", "nixos-generation-40"}
	for i, e := range got {
		if e.EntryID != wantOrder[i] {
			t.Errorf("[%d] = %q, want %q", i, e.EntryID, wantOrder[i])
		}
	}
}

// ---------- versionLess ----------

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"nixos-generation-9", "nixos-generation-10", true},  // 9 < 10 numerically
		{"nixos-generation-10", "nixos-generation-9", false}, // not lexical
		{"23.05", "23.11", true},
		{"23.11", "24.05", true},
		{"nixos", "ubuntu", true},
		{"a", "aa", true}, // shorter prefix sorts first
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// ---------- globMatch ----------

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"nixos-generation-*", "nixos-generation-42", true},
		{"nixos-generation-*", "nixos-generation-", true}, // empty match
		{"nixos-generation-*", "ubuntu-22.04", false},
		{"*", "anything", true},
		{"*.conf", "foo.conf", true},
		{"*.conf", "foo.cfg", false},
		{"?ixos", "nixos", true},
		{"?ixos", "nnixos", false}, // single char only
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}
