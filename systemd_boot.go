// Package systemdboot is a pure-Go reader for systemd-boot
// configurations following the Boot Loader Specification:
// https://uapi-group.org/specifications/specs/boot_loader_specification/
//
// The package solves kernel discovery for Linux installs that ship
// the kernel + initrd at content-addressed paths in
// /EFI/<distro>/<hash>-* (NixOS, Clear Linux, Fedora CoreOS w/
// systemd-boot, custom Arch/Gentoo setups). The conventional
// /boot/{vmlinuz,Image}-* glob doesn't find them; this package
// reads /loader/loader.conf + /loader/entries/*.conf and returns
// the resolved linux/initrd paths.
//
// The core BLS reader does no I/O of its own — callers pass an FS
// abstraction that any of go-filesystems' concrete readers satisfies via
// Go's structural typing.
//
// On top of the reader, this package is a production consumer of the pure-Go
// storage / firmware / boot stack:
//
//   - OpenImage (image.go) GPT-locates the EFI System Partition with
//     go-volumes/gpt, mounts its FAT32 filesystem with go-filesystems/detect,
//     adapts it to FS, and returns resolved BLS entries.
//   - ReadBootState and the BootNext/BootOrder helpers (uefi.go) read and
//     manage the UEFI boot variables via go-filesystems/uefi.
//   - MeasureBoot (measure.go) measures the selected kernel/initrd/cmdline
//     into the conventional TPM PCRs via go-tpm2, behind an injected Extender
//     so the package stays TPM-free when no TPM is present.
package systemdboot

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// FS is the minimal filesystem surface the parser needs. Any type
// matching this signature (in particular the concrete drivers from
// go-filesystems) satisfies it without explicit declaration.
type FS interface {
	ReadFile(path string) ([]byte, error)
	ListDir(path string) ([]string, error)
}

// Boot is a resolved boot entry — the BLS .conf file's content with
// the most useful fields lifted out.
type Boot struct {
	// EntryID is the filename of the .conf entry without the
	// ".conf" suffix, e.g. "nixos-generation-42".
	EntryID string

	// Title is the human-readable name for the entry, e.g. "NixOS".
	Title string

	// Version sorts entries within the same sort-key group, e.g.
	// "Generation 42 NixOS 23.11.20240115.abc".
	Version string

	// SortKey groups entries across distros (systemd-boot sorts
	// entries with the same sort-key together, then by version).
	// Empty if not set in the entry file.
	SortKey string

	// LinuxPath is the linux= field — the kernel binary's path
	// relative to the ESP root, e.g. "/EFI/nixos/<hash>-linux.efi".
	LinuxPath string

	// InitrdPath is the initrd= field, similar.
	InitrdPath string

	// Options is the kernel command line, concatenated from any
	// options= lines in the entry file (BLS allows multiple).
	Options string

	// Raw retains the unparsed key/value pairs the parser saw — useful
	// for less-common keys (machine-id, devicetree, efi, architecture).
	Raw map[string]string
}

// LoaderConf is the parsed /loader/loader.conf — currently only
// `default` and `timeout` are surfaced (the rest are UI knobs we
// don't care about as a non-interactive reader).
type LoaderConf struct {
	Default string // possibly with a "*" suffix wildcard
	Timeout string // string form, e.g. "5" or "menu-force"
	Raw     map[string]string
}

// ErrNotFound is returned when the requested configuration file
// doesn't exist on the FS.
var ErrNotFound = fmt.Errorf("systemd-boot: not found")

// Default reads <espRoot>/loader/loader.conf and the entry it
// points at, returning the resolved Boot. If loader.conf's `default`
// is a wildcard (e.g. "nixos-generation-*"), the entry with the
// highest BLS sort order wins.
func Default(fs FS, espRoot string) (*Boot, error) {
	lc, err := LoadLoaderConf(fs, espRoot)
	if err != nil {
		return nil, err
	}
	if lc.Default == "" {
		// No default → take the BLS-highest entry (matches what
		// systemd-boot itself does when no default is configured).
		entries, err := All(fs, espRoot)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, fmt.Errorf("systemd-boot: no entries under %s/loader/entries/", espRoot)
		}
		return entries[0], nil
	}
	return resolveDefault(fs, espRoot, lc.Default)
}

// LoadLoaderConf reads <espRoot>/loader/loader.conf and parses it.
// Returns ErrNotFound if the file is absent.
func LoadLoaderConf(fs FS, espRoot string) (*LoaderConf, error) {
	data, err := fs.ReadFile(path.Join(espRoot, "loader", "loader.conf"))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	pairs, err := parseKV(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return &LoaderConf{
		Default: stripConf(pairs["default"]),
		Timeout: pairs["timeout"],
		Raw:     pairs,
	}, nil
}

// All returns every entry under <espRoot>/loader/entries/, sorted
// by BLS rules (sort-key descending, then version descending).
func All(fs FS, espRoot string) ([]*Boot, error) {
	entriesDir := path.Join(espRoot, "loader", "entries")
	names, err := fs.ListDir(entriesDir)
	if err != nil {
		return nil, fmt.Errorf("%w: list %s: %v", ErrNotFound, entriesDir, err)
	}
	out := make([]*Boot, 0, len(names))
	for _, name := range names {
		if !strings.HasSuffix(name, ".conf") {
			continue
		}
		entry, err := loadEntry(fs, path.Join(entriesDir, name))
		if err != nil {
			// Skip unreadable entries — they're noise, not a hard fail.
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return blsLess(out[j], out[i]) // descending
	})
	return out, nil
}

// resolveDefault expands a default pattern (possibly with "*"
// wildcard) and returns the BLS-highest matching entry.
func resolveDefault(fs FS, espRoot, pattern string) (*Boot, error) {
	if !strings.Contains(pattern, "*") {
		// Literal match — read the file directly.
		entryPath := path.Join(espRoot, "loader", "entries", pattern+".conf")
		entry, err := loadEntry(fs, entryPath)
		if err != nil {
			return nil, fmt.Errorf("default entry %q: %w", pattern, err)
		}
		return entry, nil
	}
	all, err := All(fs, espRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range all {
		if globMatch(pattern, e.EntryID) {
			return e, nil
		}
	}
	return nil, fmt.Errorf("no entry matches default pattern %q", pattern)
}

// loadEntry reads and parses one /loader/entries/<id>.conf file.
func loadEntry(fs FS, entryPath string) (*Boot, error) {
	data, err := fs.ReadFile(entryPath)
	if err != nil {
		return nil, err
	}
	pairs, opts, err := parseEntry(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", entryPath, err)
	}
	id := strings.TrimSuffix(path.Base(entryPath), ".conf")
	return &Boot{
		EntryID:    id,
		Title:      pairs["title"],
		Version:    pairs["version"],
		SortKey:    pairs["sort-key"],
		LinuxPath:  pairs["linux"],
		InitrdPath: pairs["initrd"],
		Options:    opts,
		Raw:        pairs,
	}, nil
}

// parseKV reads a stream of `key value` lines (whitespace-separated,
// `#`-introduced comments, blank lines OK) into a map. The last
// occurrence of any key wins, matching systemd's own behaviour for
// loader.conf.
func parseKV(r io.Reader) (map[string]string, error) {
	pairs := make(map[string]string)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		key, val, ok := splitFirstWS(line)
		if !ok {
			continue
		}
		pairs[strings.ToLower(key)] = strings.TrimSpace(val)
	}
	return pairs, scanner.Err()
}

// parseEntry is like parseKV but treats `options` specially per
// the BLS spec: multiple `options` lines are concatenated with a
// single space between them, becoming one Boot.Options string.
func parseEntry(r io.Reader) (map[string]string, string, error) {
	pairs := make(map[string]string)
	var opts []string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		key, val, ok := splitFirstWS(line)
		if !ok {
			continue
		}
		k := strings.ToLower(key)
		v := strings.TrimSpace(val)
		if k == "options" {
			opts = append(opts, v)
			continue
		}
		pairs[k] = v
	}
	return pairs, strings.Join(opts, " "), scanner.Err()
}

// splitFirstWS splits a line at the first run of whitespace. Returns
// (key, value, true) on success, (line, "", false) if no whitespace
// is present (i.e. the line is a single token with no value).
func splitFirstWS(line string) (string, string, bool) {
	idx := strings.IndexAny(line, " \t")
	if idx < 0 {
		return line, "", false
	}
	return line[:idx], strings.TrimLeft(line[idx:], " \t"), true
}

// stripConf removes a trailing ".conf" suffix from a loader.conf
// `default` value. systemd-boot accepts both `default foo` and
// `default foo.conf`; we normalise to the .conf-less form.
func stripConf(s string) string {
	return strings.TrimSuffix(s, ".conf")
}

// blsLess returns true iff a sorts before b under the Boot Loader
// Specification rules: by sort-key first (versionsort, ascending),
// then by version (versionsort, ascending). Entries with no
// sort-key go after entries that have one; among those, EntryID is
// the tie-breaker.
//
// The caller flips the result to get descending order (newest first).
func blsLess(a, b *Boot) bool {
	if a.SortKey != b.SortKey {
		if a.SortKey == "" {
			return false // empty sort-key sorts last
		}
		if b.SortKey == "" {
			return true
		}
		return versionLess(a.SortKey, b.SortKey)
	}
	if a.Version != b.Version {
		return versionLess(a.Version, b.Version)
	}
	return versionLess(a.EntryID, b.EntryID)
}

// versionLess is a minimal strverscmp-like comparison: digit runs
// compare numerically, non-digit runs lexicographically. Enough to
// order "nixos-generation-9" before "nixos-generation-10", and
// "23.11" before "24.05". The full glibc strverscmp has more
// nuance (leading-zero handling for fractional numbers) but no BLS
// entry name needs that today.
func versionLess(a, b string) bool {
	for {
		// Skip past matching non-digit prefixes.
		i := 0
		for i < len(a) && i < len(b) && !isDigit(a[i]) && !isDigit(b[i]) && a[i] == b[i] {
			i++
		}
		if i == len(a) || i == len(b) {
			return len(a) < len(b)
		}
		if !isDigit(a[i]) || !isDigit(b[i]) {
			return a[i] < b[i]
		}
		// Both at a digit — extract the full digit runs and
		// compare numerically.
		aEnd := i
		for aEnd < len(a) && isDigit(a[aEnd]) {
			aEnd++
		}
		bEnd := i
		for bEnd < len(b) && isDigit(b[bEnd]) {
			bEnd++
		}
		aNum := stripLeadingZeros(a[i:aEnd])
		bNum := stripLeadingZeros(b[i:bEnd])
		if len(aNum) != len(bNum) {
			return len(aNum) < len(bNum)
		}
		if aNum != bNum {
			return aNum < bNum
		}
		// Equal up to here; continue past both digit runs.
		a = a[aEnd:]
		b = b[bEnd:]
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func stripLeadingZeros(s string) string {
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	return s
}

// globMatch implements the systemd-boot `default` wildcard — a
// single `*` matches any run of characters, `?` matches one. We do
// not support character classes (`[abc]`) since systemd-boot doesn't
// either.
func globMatch(pattern, name string) bool {
	pi, ni := 0, 0
	star, starN := -1, 0
	for ni < len(name) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == name[ni]):
			pi++
			ni++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			starN = ni
			pi++
		case star >= 0:
			pi = star + 1
			starN++
			ni = starN
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
