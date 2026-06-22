// measure.go implements the measured-boot helper: given a resolved BLS Boot
// entry and the ESP it was read from, it measures the entry's kernel, initrd
// and kernel command line into the conventional TPM PCRs and emits the
// matching TCG event-log entries.
//
// systemd-boot / sd-stub perform these measurements during boot. The PCR
// registry follows the systemd TPM2 PCR assignments
// (https://uapi-group.org/specifications/specs/linux_tpm_pcr_registry/):
//
//	PCR 4  — boot-loader code + the kernel image it hands off to
//	PCR 8  — kernel command line (GRUB-style; sd-boot also records cmdline)
//	PCR 9  — initrd (and other files the loader reads)
//	PCR 11 — unified-kernel-image (UKI / sd-stub) PE sections
//
// Measurement is optional and gated behind an injected Extender, so a caller
// with no TPM never touches one: pass a real *tpm2.TPM (over a crb/tis
// transport) to measure into hardware, or any Extender fake to dry-run.

package systemdboot

import (
	"crypto/sha256"
	"fmt"

	"github.com/go-tpm2/attest"
	"github.com/go-tpm2/common"
)

// Conventional PCR indices used by the systemd measured-boot chain. They are
// literals here because the go-tpm2 libraries deliberately do not encode the
// systemd registry — the mapping is policy, supplied by the consumer.
const (
	// PCRKernel is the PCR into which the kernel image is measured (the
	// boot-loader / loaded-image PCR).
	PCRKernel = 4
	// PCRCmdline is the PCR into which the kernel command line is measured.
	PCRCmdline = 8
	// PCRInitrd is the PCR into which the initrd is measured.
	PCRInitrd = 9
	// PCRUKI is the PCR into which unified-kernel-image (sd-stub) sections
	// are measured.
	PCRUKI = 11
)

// TCG event types used for the emitted log entries. Values from the TCG PC
// Client Platform Firmware Profile (EV_* registry).
const (
	evEFIBootServicesApplication uint32 = 0x80000003 // kernel image (EV_EFI_BOOT_SERVICES_APPLICATION)
	evIPL                        uint32 = 0x0000000D // initrd / loaded payload (EV_IPL)
	evIPLPartitionData           uint32 = 0x0000000E // cmdline (EV_IPL_PARTITION_DATA)
)

// Extender is the minimal TPM surface the measured-boot helper needs: extend
// a digest into a PCR bank. *github.com/go-tpm2/tpm2.TPM satisfies it
// (PCRExtend(pcr int, hash uint16, digest []byte) error), so a caller passes a
// real TPM to measure into hardware, or a fake for dry-runs / tests. Keeping
// the dependency behind this interface is what lets the package build and run
// with no TPM present.
type Extender interface {
	PCRExtend(pcr int, hash uint16, digest []byte) error
}

// Measurement is one PCR extension that was (or would be) performed: the PCR
// index, the SHA-256 digest extended into it, the TCG event type, and a human
// description of what was measured.
type Measurement struct {
	PCR         int
	Digest      [32]byte
	EventType   uint32
	Description string
}

// MeasureResult is the outcome of measuring a Boot entry: the ordered list of
// measurements performed and the serialised TCG event log describing them.
type MeasureResult struct {
	Measurements []Measurement
	EventLog     []byte
}

// MeasureBoot measures the selected Boot entry into the conventional PCRs via
// ext and returns the measurements plus a TCG event log. The kernel and initrd
// are read from the mounted ESP (im); the command line is taken from the
// entry's Options. fileLimit bounds each file read (see Image.ReadESPFile).
//
// ext may be nil for a pure event-log dry run (digests are still computed and
// logged, but nothing is extended into a TPM) — this is the TPM-free path. To
// measure into hardware, pass a *tpm2.TPM built over a crb/tis transport.
//
// The digest of each measured object is SHA-256 over its bytes, which is the
// digest the event log records and what a SHA-256 PCR bank is extended with.
func MeasureBoot(im *Image, entry *Boot, ext Extender, fileLimit int64) (*MeasureResult, error) {
	if entry == nil {
		return nil, fmt.Errorf("systemd-boot: nil boot entry")
	}

	log := attest.NewLogBuilder()
	res := &MeasureResult{}

	add := func(pcr int, etype uint32, desc string, data []byte) error {
		sum := sha256.Sum256(data)
		if ext != nil {
			if err := ext.PCRExtend(pcr, uint16(common.AlgSHA256), sum[:]); err != nil {
				return fmt.Errorf("systemd-boot: extend PCR %d (%s): %w", pcr, desc, err)
			}
		}
		log.Add(pcr, etype, sum[:], data)
		res.Measurements = append(res.Measurements, Measurement{
			PCR:         pcr,
			Digest:      sum,
			EventType:   etype,
			Description: desc,
		})
		return nil
	}

	// Kernel image → PCR 4.
	if entry.LinuxPath != "" {
		kernel, err := im.ReadESPFile(entry.LinuxPath, fileLimit)
		if err != nil {
			return nil, err
		}
		if err := add(PCRKernel, evEFIBootServicesApplication, "kernel "+entry.LinuxPath, kernel); err != nil {
			return nil, err
		}
	}

	// initrd → PCR 9.
	if entry.InitrdPath != "" {
		initrd, err := im.ReadESPFile(entry.InitrdPath, fileLimit)
		if err != nil {
			return nil, err
		}
		if err := add(PCRInitrd, evIPL, "initrd "+entry.InitrdPath, initrd); err != nil {
			return nil, err
		}
	}

	// Kernel command line → PCR 8. Measured as the NUL-terminated string the
	// loader passes, matching how sd-boot records the cmdline event.
	if entry.Options != "" {
		cmdline := append([]byte(entry.Options), 0)
		if err := add(PCRCmdline, evIPLPartitionData, "cmdline", cmdline); err != nil {
			return nil, err
		}
	}

	res.EventLog = log.Bytes()
	return res, nil
}
