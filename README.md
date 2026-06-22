# systemd-boot

[![ci](https://github.com/go-bootloaders/systemd-boot/actions/workflows/ci.yml/badge.svg)](https://github.com/go-bootloaders/systemd-boot/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-bootloaders/systemd-boot.svg)](https://pkg.go.dev/github.com/go-bootloaders/systemd-boot)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

Pure-Go (`CGO_ENABLED=0`) reader and integration layer for
[systemd-boot](https://www.freedesktop.org/software/systemd/man/systemd-boot.html),
following the
[Boot Loader Specification](https://uapi-group.org/specifications/specs/boot_loader_specification/).

It is a production-ready consumer of the pure-Go storage / firmware / boot
stack: point it at a **whole disk image** and it GPT-locates the EFI System
Partition, mounts its FAT32 filesystem, and returns resolved BLS boot entries
— then optionally reads the UEFI boot-variable state and measures the selected
kernel/initrd/cmdline into TPM PCRs, all with no cgo and no shell-outs.

## What it does

| Capability | Backed by |
|---|---|
| Parse `/loader/loader.conf` + `/loader/entries/*.conf` → resolved `Boot` entries (linux/initrd/options, BLS sort order, wildcard `default`) | this package |
| Disk image → ESP → resolved entries with zero manual FS plumbing | [`go-volumes/gpt`](https://github.com/go-volumes/gpt) + [`go-filesystems/detect`](https://github.com/go-filesystems/detect) (`fat32reg`) |
| Read UEFI `BootOrder` / `Boot####` LoadOptions (device-path text), set `BootNext` / `BootOrder`, find which `Boot####` is systemd-boot | [`go-filesystems/uefi`](https://github.com/go-filesystems/uefi) |
| Measured boot: extend kernel (PCR 4) / cmdline (PCR 8) / initrd (PCR 9) into a TPM + emit a TCG event log | [`go-tpm2`](https://github.com/go-tpm2) (`tpm2`, `attest`, `common`) |

## Module

```text
github.com/go-bootloaders/systemd-boot
```

## Disk-image → BLS entries

`OpenImage` does the whole pipeline — GPT-locate the ESP, detect + mount its
FAT32 filesystem, adapt it to the reader, parse `loader.conf` — behind a single
call. The caller MUST `Close` the returned `Image`.

```go
im, err := systemdboot.OpenImage("/dev/sda")     // or a disk-image file
if err != nil { /* ... */ }
defer im.Close()

def, _ := im.Default()        // the active entry (loader.conf default, or BLS-highest)
all, _ := im.Entries()        // every entry, newest first
// def.LinuxPath  = "/EFI/nixos/<hash>-linux.efi"
// def.InitrdPath = "/EFI/nixos/<hash>-initrd"
// def.Options    = "init=/nix/store/… root=/dev/disk/by-label/nixos"
```

The `Image.FS` field is the mounted ESP adapted to the minimal `FS` interface
(`ReadFile` + `ListDir`). Callers who **already** hold a mounted ESP filesystem
(any `go-filesystems` driver, i.e. a `filesystem.Filesystem`) skip the disk
plumbing with `NewFS`:

```go
fs := systemdboot.NewFS(mountedESP)          // filesystem.Filesystem → FS
boot, _ := systemdboot.Default(fs, "/")      // espRoot within the mount
```

`NewFS` is the adapter that bridges the driver's `ListDir`
(`[]filesystem.DirEntry`) to the reader's `ListDir` (`[]string`).

## UEFI boot-variable state

systemd-boot installs itself as a UEFI `Boot####` LoadOption; the BLS entries
it then presents are a separate stage. `ReadBootState` reads the firmware side
from a variable store (a real `OVMF_VARS.fd`, or one minted with
`go-filesystems/uefi`):

```go
store, _ := uefi.Open("OVMF_VARS.fd")
defer store.Close()

state, _ := systemdboot.ReadBootState(store)
// state.Order              — the firmware BootOrder
// state.Entries[n]         — Boot####n: Description, DevicePathText, Active, …
// state.SystemdBootSlots   — which Boot#### point at systemd-boot
// state.BootNext           — one-shot next boot, if set

systemdboot.SetBootNext(store, state.SystemdBootSlots[0])  // boot sd-boot once
systemdboot.MakeSystemdBootFirst(store)                    // make it default
```

An entry is recognised as systemd-boot when its device path's `File()` node is
`\EFI\systemd\systemd-boot{x64,aa64}.efi` or the removable-media fallback
`\EFI\BOOT\BOOT{X64,AA64}.EFI`.

## Measured boot (optional, TPM-free by default)

Given a selected entry and the mounted ESP, `MeasureBoot` reads the kernel and
initrd from the ESP, hashes them and the command line, extends them into the
conventional [systemd PCR registry](https://uapi-group.org/specifications/specs/linux_tpm_pcr_registry/)
indices, and returns a TCG event log:

```go
// nil Extender → pure event-log dry run, no TPM touched.
res, _ := systemdboot.MeasureBoot(im, def, nil, 512<<20)

// or measure into hardware over a crb/tis transport:
tpm := tpm2.New(transport)
res, _ := systemdboot.MeasureBoot(im, def, tpm, 512<<20)
// res.Measurements — PCR 4 kernel, PCR 8 cmdline, PCR 9 initrd (digest + event type)
// res.EventLog     — serialised TCG event log (parse with attest.ParseEventLog)
```

The TPM dependency sits behind the injected `Extender` interface
(`PCRExtend(pcr int, hash uint16, digest []byte) error`, satisfied by
`*tpm2.TPM`), so the tool builds and runs with no TPM present.

## Why this exists

The conventional `/boot/{vmlinuz,Image}-*` glob misses **every systemd-boot
install** — NixOS, Clear Linux, and bespoke Arch / Gentoo setups put the kernel
at content-addressed `/EFI/<distro>/<hash>-*` paths selected through
`/loader/entries/*.conf`. Reading the canonical BLS path covers the whole
ecosystem at once, and wiring it to the storage / firmware / TPM stack turns it
into a complete, pure-Go systemd-boot inspector for the cloud-boot / weft
toolchain.

## Building

```text
GOWORK=off go build ./...
```

`go-filesystems/interface` is consumed via a sibling `replace => ../interface`
(its drivers reference it the same way, and that directive does not survive
transitive importing). CI checks the repo out flat next to this module; for
local development clone `go-filesystems/interface` next to a `../interface`
path. All other dependencies resolve from the public module proxy.

Validated on all six 64-bit Go architectures
(amd64 / arm64 / riscv64 / loong64 / ppc64le / s390x, the last big-endian).

## License

BSD-3-Clause. See [LICENSE](LICENSE).
