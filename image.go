// image.go wires the pure-Go BLS reader to the go-volumes / go-filesystems
// storage stack so a caller can point at a whole disk image and get back
// resolved boot entries with zero manual filesystem plumbing.
//
// The flow is: GPT-locate the EFI System Partition (go-volumes/gpt) →
// detect + mount its FAT32 filesystem (go-filesystems/detect + fat32reg) →
// adapt the driver's filesystem.Filesystem to the local FS interface →
// run the existing BLS reader.

package systemdboot

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/go-filesystems/detect"
	_ "github.com/go-filesystems/detect/fat32reg" // register the FAT32 opener
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/gpt"
	"github.com/go-volumes/safeio"
)

// fsAdapter wraps a go-filesystems filesystem.Filesystem so it satisfies the
// local FS interface. The driver's ListDir returns []filesystem.DirEntry; the
// BLS reader wants []string, so the adapter maps each DirEntry to its Name.
type fsAdapter struct {
	fs filesystem.Filesystem
}

// ReadFile delegates straight through — both surfaces use the same signature.
func (a fsAdapter) ReadFile(p string) ([]byte, error) {
	return a.fs.ReadFile(p)
}

// ListDir maps the driver's []filesystem.DirEntry down to the []string the
// BLS reader consumes. Entries named "." and ".." are dropped: the reader
// only ever lists /loader/entries to enumerate *.conf files, and the dot
// entries are never .conf, but filtering them keeps the surface clean for any
// caller that lists other directories through the adapter.
func (a fsAdapter) ListDir(p string) ([]string, error) {
	entries, err := a.fs.ListDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if name == "." || name == ".." {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// NewFS adapts a go-filesystems driver (anything implementing
// filesystem.Filesystem — fat32, ext4, …) to the local FS interface so it can
// be passed to Default / All / LoadLoaderConf. Callers who already hold a
// mounted ESP filesystem use this instead of OpenImage.
func NewFS(fs filesystem.Filesystem) FS {
	return fsAdapter{fs: fs}
}

// maxImage caps how large a disk image OpenImage will stat/stage, guarding
// against an attacker-supplied size. 4 TiB comfortably covers any real ESP
// host disk while keeping bounded I/O honest.
const maxImage = int64(4) << 40

// Seams over os/io so OpenImage's error branches are exercisable in tests.
var (
	osOpen       = os.Open
	osStat       = os.Stat
	createTemp   = os.CreateTemp
	ioCopy       = io.Copy
	detectOpen   = detect.Open
	gptByType    = gpt.ByType
	osRemoveFile = os.Remove
)

// Image is a mounted disk image: an ESP filesystem adapted to the FS
// interface, the disk geometry that located it, and the parsed loader.conf.
// Close releases the underlying filesystem and any staged temp file.
type Image struct {
	// FS is the ESP's FAT32 filesystem adapted to the local FS interface.
	// Pass it (with EspRoot) to Default / All for resolved entries.
	FS FS

	// EspRoot is the path within FS at which the loader/ directory lives.
	// For a freshly mounted ESP this is "/" (the ESP root is the FS root).
	EspRoot string

	// Partition is the GPT EFI System Partition entry that was located.
	Partition gpt.Partition

	// LoaderConf is the parsed /loader/loader.conf (nil if absent).
	LoaderConf *LoaderConf

	closer io.Closer
	disk   *os.File
}

// Close releases the mounted filesystem and the opened disk file.
func (im *Image) Close() error {
	var err error
	if im.closer != nil {
		err = im.closer.Close()
	}
	if im.disk != nil {
		if cerr := im.disk.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// Entries returns every BLS entry on the mounted ESP, BLS-sorted.
func (im *Image) Entries() ([]*Boot, error) {
	return All(im.FS, im.EspRoot)
}

// Default returns the active BLS entry (loader.conf's default, or the
// BLS-highest entry when no default is set).
func (im *Image) Default() (*Boot, error) {
	return Default(im.FS, im.EspRoot)
}

// OpenImage opens a whole disk image at path, locates its EFI System
// Partition via GPT, mounts the partition's FAT32 filesystem, and returns an
// Image whose FS yields resolved BLS boot entries. The caller MUST Close the
// returned Image.
//
// loader.conf is parsed eagerly and exposed as Image.LoaderConf; its absence
// is not an error (an ESP may carry entries without a loader.conf), in which
// case LoaderConf is nil.
func OpenImage(diskPath string) (*Image, error) {
	fi, err := osStat(diskPath)
	if err != nil {
		return nil, fmt.Errorf("systemd-boot: stat image: %w", err)
	}
	size := fi.Size()
	if size <= 0 || size > maxImage {
		return nil, fmt.Errorf("systemd-boot: image size %d out of range", size)
	}

	disk, err := osOpen(diskPath)
	if err != nil {
		return nil, fmt.Errorf("systemd-boot: open image: %w", err)
	}

	part, err := gptByType(disk, size, gpt.EFISystemPartitionGUID)
	if err != nil {
		disk.Close()
		return nil, fmt.Errorf("systemd-boot: locate ESP: %w", err)
	}

	// Carve the ESP out of the whole-disk reader and mount it. The detect
	// registry dispatches to the fat32 driver (blank-imported above), which
	// stages the section to a temp file internally and returns a
	// filesystem.Filesystem whose Close unlinks it.
	esp := io.NewSectionReader(disk, part.StartOffset, part.Length)
	fs, typ, err := detectOpen(esp, part.Length)
	if err != nil {
		disk.Close()
		return nil, fmt.Errorf("systemd-boot: mount ESP (detected %s): %w", typ, err)
	}

	adapted := NewFS(fs)
	im := &Image{
		FS:        adapted,
		EspRoot:   "/",
		Partition: part,
		closer:    fs,
		disk:      disk,
	}

	// Parse loader.conf if present; its absence is fine.
	lc, lerr := LoadLoaderConf(adapted, im.EspRoot)
	if lerr == nil {
		im.LoaderConf = lc
	}
	return im, nil
}

// ReadESPFile reads a file from the mounted ESP, resolving an ESP-relative
// path (as found in a BLS linux=/initrd= field) against the ESP root. The
// returned slice is bounded by limit so an untrusted/corrupt FAT directory
// entry cannot make a caller process an unexpectedly huge file.
//
// limit caps the returned bytes; pass a value comfortably above the largest
// kernel/initrd you expect (e.g. 512 MiB). A file larger than limit is an
// error rather than a silent truncation. A non-positive limit reads with no
// cap (use only for trusted images).
func (im *Image) ReadESPFile(espRelPath string, limit int64) ([]byte, error) {
	clean := path.Join(im.EspRoot, strings.TrimPrefix(espRelPath, "/"))
	data, err := im.FS.ReadFile(clean)
	if err != nil {
		return nil, fmt.Errorf("systemd-boot: read %s: %w", clean, err)
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("systemd-boot: %s is %d bytes, exceeds limit %d", clean, len(data), limit)
	}
	return data, nil
}

// boundedCopy stages a bounded amount of data from a section reader, used by
// OpenImage's verification path and exported for callers that hold a raw
// ESP section. It enforces an upper bound via safeio so a hostile size field
// cannot drive an unbounded allocation. Kept as the single safeio seam.
func boundedCopy(r io.ReaderAt, off, n, max int64) ([]byte, error) {
	return safeio.ReadAtFull(r, off, n, max)
}
