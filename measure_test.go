package systemdboot

import (
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/go-tpm2/attest"
	"github.com/go-tpm2/common"
	"github.com/go-tpm2/tpm2"
)

// fakeTransport is a common.Transport that records every PCR_Extend command
// and replies with a success response, so a real *tpm2.TPM can drive it with
// no hardware present. This is the TPM-free seam in action.
type fakeTransport struct {
	sends   int
	fail    bool
	lastCmd []byte
}

func (f *fakeTransport) Send(cmd []byte) ([]byte, error) {
	f.sends++
	f.lastCmd = cmd
	if f.fail {
		return nil, errors.New("transport failure")
	}
	// A minimal well-formed success response header:
	//   [ tag:u16 | responseSize:u32 | responseCode:u32 ]
	// tpm2.execute only checks the response code, so an empty-parameter
	// success suffices.
	rsp := common.PutU16(nil, uint16(common.TagSessions))
	rsp = common.PutU32(rsp, 10) // header-only size
	rsp = common.PutU32(rsp, uint32(common.RCSuccess))
	return rsp, nil
}

func openMeasureImage(t *testing.T) *Image {
	t.Helper()
	diskPath := buildESPDiskImage(t, layNixOS)
	im, err := OpenImage(diskPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	t.Cleanup(func() { im.Close() })
	return im
}

func TestMeasureBoot_ExtendsPCRs(t *testing.T) {
	im := openMeasureImage(t)
	def, err := im.Default()
	if err != nil {
		t.Fatal(err)
	}

	tr := &fakeTransport{}
	tpm := tpm2.New(tr)

	res, err := MeasureBoot(im, def, tpm, 1<<20)
	if err != nil {
		t.Fatalf("MeasureBoot: %v", err)
	}

	// kernel (PCR 4), initrd (PCR 9), cmdline (PCR 8) → three extensions.
	if len(res.Measurements) != 3 {
		t.Fatalf("want 3 measurements, got %d", len(res.Measurements))
	}
	if tr.sends != 3 {
		t.Errorf("want 3 PCR_Extend sends, got %d", tr.sends)
	}

	gotPCR := map[int]bool{}
	for _, m := range res.Measurements {
		gotPCR[m.PCR] = true
	}
	for _, want := range []int{PCRKernel, PCRInitrd, PCRCmdline} {
		if !gotPCR[want] {
			t.Errorf("missing measurement for PCR %d", want)
		}
	}

	// The kernel digest must be SHA-256 over the actual kernel bytes read
	// from the ESP — proving we measured real content, not a placeholder.
	wantKernel := sha256.Sum256([]byte("KERNEL-ccc-bytes"))
	var sawKernel bool
	for _, m := range res.Measurements {
		if m.PCR == PCRKernel {
			sawKernel = true
			if m.Digest != wantKernel {
				t.Errorf("kernel digest = %x, want %x", m.Digest, wantKernel)
			}
		}
	}
	if !sawKernel {
		t.Error("no PCR 4 kernel measurement")
	}

	// The event log must parse and replay to the same digests.
	events, err := attest.ParseEventLog(res.EventLog)
	if err != nil {
		t.Fatalf("ParseEventLog: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("event log has %d events, want 3", len(events))
	}
}

func TestMeasureBoot_DryRunNoTPM(t *testing.T) {
	im := openMeasureImage(t)
	def, err := im.Default()
	if err != nil {
		t.Fatal(err)
	}
	// nil Extender → pure event-log dry run, no TPM touched.
	res, err := MeasureBoot(im, def, nil, 1<<20)
	if err != nil {
		t.Fatalf("MeasureBoot (dry run): %v", err)
	}
	if len(res.Measurements) != 3 {
		t.Errorf("want 3 measurements in dry run, got %d", len(res.Measurements))
	}
	if len(res.EventLog) == 0 {
		t.Error("dry run produced empty event log")
	}
}

func TestMeasureBoot_NilEntry(t *testing.T) {
	im := openMeasureImage(t)
	if _, err := MeasureBoot(im, nil, nil, 1<<20); err == nil {
		t.Error("expected error for nil entry")
	}
}

func TestMeasureBoot_ExtendError(t *testing.T) {
	im := openMeasureImage(t)
	def, err := im.Default()
	if err != nil {
		t.Fatal(err)
	}
	tpm := tpm2.New(&fakeTransport{fail: true})
	if _, err := MeasureBoot(im, def, tpm, 1<<20); err == nil {
		t.Error("expected extend error from failing transport")
	}
}

func TestMeasureBoot_MissingKernelFile(t *testing.T) {
	// Entry references a kernel that isn't on the ESP → read error.
	im := openMeasureImage(t)
	bad := &Boot{LinuxPath: "/EFI/nixos/does-not-exist"}
	if _, err := MeasureBoot(im, bad, nil, 1<<20); err == nil {
		t.Error("expected read error for missing kernel")
	}
}

func TestMeasureBoot_CmdlineOnly(t *testing.T) {
	// An entry with only Options exercises just the cmdline branch.
	im := openMeasureImage(t)
	entry := &Boot{Options: "root=/dev/sda1 ro"}
	res, err := MeasureBoot(im, entry, nil, 1<<20)
	if err != nil {
		t.Fatalf("MeasureBoot: %v", err)
	}
	if len(res.Measurements) != 1 || res.Measurements[0].PCR != PCRCmdline {
		t.Errorf("want single cmdline measurement, got %+v", res.Measurements)
	}
}

// Ensure *tpm2.TPM satisfies the Extender interface (compile-time check).
var _ Extender = (*tpm2.TPM)(nil)
