module github.com/go-bootloaders/systemd-boot

go 1.26.4

require (
	github.com/go-filesystems/detect v0.0.0
	github.com/go-filesystems/detect/fat32reg v0.0.0-20260622100514-ad3c237ff7a8
	github.com/go-filesystems/fat32 v0.0.0
	github.com/go-filesystems/interface v0.0.0
	github.com/go-filesystems/uefi v0.0.0-20260622100157-d19717ea67ff
	github.com/go-tpm2/attest v0.3.0
	github.com/go-tpm2/common v0.1.0
	github.com/go-volumes/gpt v0.0.0-20260622100756-3721db1fbd05
	github.com/go-volumes/safeio v0.0.0-20260622072324-7f8eb19f6f8c
)

require github.com/go-tpm2/tpm2 v0.6.0

// go-filesystems/interface ships only the placeholder v0.0.0 (its drivers
// reference it via a sibling `replace => ../interface`), so as the importing
// main module we provide that replace ourselves. CI checks the repo out flat
// next to this module as ../interface.
replace github.com/go-filesystems/interface => ../interface

// detect/fat32reg (blank-imported to register the FAT32 opener) requires
// detect and fat32 at the placeholder v0.0.0; pin them to real proxy
// pseudo-versions here so the meta-import resolves (the drivers' own sibling
// replaces do not compose for a downstream importer).
replace github.com/go-filesystems/detect => github.com/go-filesystems/detect v0.0.0-20260622100514-ad3c237ff7a8

replace github.com/go-filesystems/fat32 => github.com/go-filesystems/fat32 v0.0.0-20260622082158-99c94157eb55
