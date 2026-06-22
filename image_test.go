package systemdboot

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/gpt"
)

// sectorSize is the LBA size the test GPT uses (matches gpt.SectorSize).
const sectorSize = 512

// buildESPDiskImage formats a FAT32 filesystem populated by lay, then wraps it
// in a GPT disk image with a single EFI System Partition. It returns the path
// to the whole-disk image. The image layout is:
//
//	LBA 0      protective MBR (type 0xEE)
//	LBA 1      primary GPT header ("EFI PART")
//	LBA 2      partition entry array (one ESP entry)
//	LBA partLBA.. the raw FAT32 bytes
//
// gpt's reader validates structure, not CRCs, so a faithful header + entry is
// sufficient to be located by gpt.ByType(EFISystemPartitionGUID).
func buildESPDiskImage(t *testing.T, lay func(fs filesystem.Filesystem)) string {
	t.Helper()
	dir := t.TempDir()

	// 1. Format a bare FAT32 image and lay down the BLS tree.
	fatPath := filepath.Join(dir, "esp.fat")
	const espSize = 16 * 1024 * 1024 // 16 MiB, multiple of 4096
	fs, err := fat32.Format(fatPath, espSize, fat32.FormatConfig{Label: "ESP"})
	if err != nil {
		t.Fatalf("fat32.Format: %v", err)
	}
	lay(fs)
	if err := fs.Close(); err != nil {
		t.Fatalf("close fat fs: %v", err)
	}
	fatBytes, err := os.ReadFile(fatPath)
	if err != nil {
		t.Fatalf("read fat image: %v", err)
	}

	// 2. Assemble the whole-disk image.
	const partLBA = 2048 // 1 MiB-aligned, conventional ESP start
	partOff := int64(partLBA) * sectorSize
	deviceSize := partOff + int64(len(fatBytes)) + sectorSize // pad a sector
	img := make([]byte, deviceSize)

	// Protective MBR.
	img[510] = 0x55
	img[511] = 0xAA
	img[446+4] = 0xEE
	binary.LittleEndian.PutUint32(img[446+8:], 1)
	binary.LittleEndian.PutUint32(img[446+12:], 0xFFFFFFFF)

	// Primary GPT header at LBA 1.
	hoff := int64(1) * sectorSize
	copy(img[hoff:], []byte("EFI PART"))
	binary.LittleEndian.PutUint64(img[hoff+72:], 2) // PartitionEntryLBA
	binary.LittleEndian.PutUint32(img[hoff+80:], 1) // NumberOfPartitionEntries
	binary.LittleEndian.PutUint32(img[hoff+84:], 128)

	// One ESP entry at LBA 2.
	endLBA := uint64(partLBA) + uint64(len(fatBytes)/sectorSize) - 1
	entry := espEntry(gpt.EFISystemPartitionGUID, partLBA, endLBA, "EFI System Partition")
	copy(img[int64(2)*sectorSize:], entry)

	// FAT32 payload at the partition's start.
	copy(img[partOff:], fatBytes)

	diskPath := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(diskPath, img, 0o600); err != nil {
		t.Fatalf("write disk image: %v", err)
	}
	return diskPath
}

// espEntry builds a 128-byte GPT partition entry.
func espEntry(typeGUID [16]byte, startLBA, endLBA uint64, name string) []byte {
	e := make([]byte, 128)
	copy(e[0:16], typeGUID[:])
	// unique partition GUID (arbitrary, non-zero so the entry is "populated")
	for i := 16; i < 32; i++ {
		e[i] = byte(i)
	}
	binary.LittleEndian.PutUint64(e[32:], startLBA)
	binary.LittleEndian.PutUint64(e[40:], endLBA)
	for i, r := range name {
		if 56+i*2+2 > len(e) {
			break
		}
		binary.LittleEndian.PutUint16(e[56+i*2:], uint16(r))
	}
	return e
}

// layNixOS writes a NixOS-style systemd-boot tree onto fs.
func layNixOS(fs filesystem.Filesystem) {
	mkdirAll(fs, "/loader/entries")
	mkdirAll(fs, "/EFI/nixos")
	_ = fs.WriteFile("/loader/loader.conf", []byte("default nixos-generation-42.conf\ntimeout 5\n"), 0o644)
	_ = fs.WriteFile("/loader/entries/nixos-generation-41.conf", []byte(
		"title NixOS\nversion Generation 41 NixOS 23.11\nsort-key nixos\n"+
			"linux /EFI/nixos/bbb-linux.efi\ninitrd /EFI/nixos/bbb-initrd\n"+
			"options init=/nix/store/bbb-init root=/dev/disk/by-label/nixos\n"), 0o644)
	_ = fs.WriteFile("/loader/entries/nixos-generation-42.conf", []byte(
		"title NixOS\nversion Generation 42 NixOS 24.05\nsort-key nixos\n"+
			"linux /EFI/nixos/ccc-linux.efi\ninitrd /EFI/nixos/ccc-initrd\n"+
			"options init=/nix/store/ccc-init root=/dev/disk/by-label/nixos\n"), 0o644)
	// Content-addressed kernel + initrd payloads.
	_ = fs.WriteFile("/EFI/nixos/ccc-linux.efi", []byte("KERNEL-ccc-bytes"), 0o644)
	_ = fs.WriteFile("/EFI/nixos/ccc-initrd", []byte("INITRD-ccc-bytes"), 0o644)
}

// mkdirAll creates each path component (the fat32 driver's MkDir is single-level).
func mkdirAll(fs filesystem.Filesystem, p string) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	cur := ""
	for _, part := range parts {
		cur += "/" + part
		_ = fs.MkDir(cur, 0o755)
	}
}

// ---------- FS adapter ----------

func TestNewFS_AdaptsListDir(t *testing.T) {
	diskPath := buildESPDiskImage(t, layNixOS)
	im, err := OpenImage(diskPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()

	names, err := im.FS.ListDir("/loader/entries")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	var sawConf bool
	for _, n := range names {
		if n == "." || n == ".." {
			t.Errorf("adapter leaked dot entry %q", n)
		}
		if n == "nixos-generation-42.conf" {
			sawConf = true
		}
	}
	if !sawConf {
		t.Errorf("expected nixos-generation-42.conf in %v", names)
	}
}

// ---------- OpenImage → resolved BLS entries ----------

func TestOpenImage_ResolvesEntries(t *testing.T) {
	diskPath := buildESPDiskImage(t, layNixOS)
	im, err := OpenImage(diskPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()

	if im.Partition.TypeGUID != gpt.EFISystemPartitionGUID {
		t.Errorf("located partition is not the ESP: %x", im.Partition.TypeGUID)
	}
	if im.LoaderConf == nil || im.LoaderConf.Default != "nixos-generation-42" {
		t.Fatalf("loader.conf default = %+v", im.LoaderConf)
	}

	def, err := im.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if def.EntryID != "nixos-generation-42" {
		t.Errorf("default EntryID = %q", def.EntryID)
	}
	if def.LinuxPath != "/EFI/nixos/ccc-linux.efi" {
		t.Errorf("LinuxPath = %q", def.LinuxPath)
	}

	entries, err := im.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].EntryID != "nixos-generation-42" {
		t.Errorf("newest first failed: %q", entries[0].EntryID)
	}
}

func TestImage_ReadESPFile(t *testing.T) {
	diskPath := buildESPDiskImage(t, layNixOS)
	im, err := OpenImage(diskPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()

	data, err := im.ReadESPFile("/EFI/nixos/ccc-linux.efi", 1<<20)
	if err != nil {
		t.Fatalf("ReadESPFile: %v", err)
	}
	if string(data) != "KERNEL-ccc-bytes" {
		t.Errorf("kernel bytes = %q", data)
	}

	// Over-limit is an error.
	if _, err := im.ReadESPFile("/EFI/nixos/ccc-linux.efi", 4); err == nil {
		t.Error("expected over-limit error")
	}
	// Missing file is an error.
	if _, err := im.ReadESPFile("/EFI/nixos/nope", 1<<20); err == nil {
		t.Error("expected missing-file error")
	}
}

// ---------- OpenImage error paths ----------

func TestOpenImage_StatError(t *testing.T) {
	if _, err := OpenImage(filepath.Join(t.TempDir(), "nonexistent.img")); err == nil {
		t.Error("expected stat error")
	}
}

func TestOpenImage_SizeOutOfRange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.img")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenImage(p); err == nil {
		t.Error("expected size-out-of-range error for empty image")
	}
}

func TestOpenImage_NoESP(t *testing.T) {
	// A non-GPT blob: gpt.ByType finds no ESP.
	p := filepath.Join(t.TempDir(), "junk.img")
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenImage(p); err == nil {
		t.Error("expected locate-ESP error")
	}
}

func TestOpenImage_OpenError(t *testing.T) {
	// Seam injection: force os.Open to fail after a successful stat.
	diskPath := buildESPDiskImage(t, layNixOS)
	orig := osOpen
	osOpen = func(string) (*os.File, error) { return nil, errors.New("boom") }
	defer func() { osOpen = orig }()
	if _, err := OpenImage(diskPath); err == nil || !strings.Contains(err.Error(), "open image") {
		t.Errorf("expected open-image error, got %v", err)
	}
}

func TestOpenImage_LoaderConfAbsent(t *testing.T) {
	// An ESP with entries but no loader.conf: LoaderConf is nil, not an error.
	diskPath := buildESPDiskImage(t, func(fs filesystem.Filesystem) {
		mkdirAll(fs, "/loader/entries")
		mkdirAll(fs, "/EFI/nixos")
		_ = fs.WriteFile("/loader/entries/nixos-generation-1.conf", []byte(
			"title NixOS\nlinux /EFI/nixos/x-linux.efi\n"), 0o644)
	})
	im, err := OpenImage(diskPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	if im.LoaderConf != nil {
		t.Errorf("expected nil LoaderConf, got %+v", im.LoaderConf)
	}
}

// ---------- boundedCopy seam ----------

func TestBoundedCopy(t *testing.T) {
	data := []byte("hello world")
	r := readerAt(data)
	got, err := boundedCopy(r, 0, int64(len(data)), 1<<20)
	if err != nil {
		t.Fatalf("boundedCopy: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("got %q", got)
	}
	// Over-cap read errors.
	if _, err := boundedCopy(r, 0, int64(len(data)), 4); err == nil {
		t.Error("expected over-cap error")
	}
}

type readerAt []byte

func (b readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
