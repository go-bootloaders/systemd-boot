module github.com/go-bootloaders/systemd-boot

go 1.26.4

require (
	github.com/go-filesystems/detect v0.1.0
	github.com/go-filesystems/detect/fat32reg v0.0.0-20260831153547-a065afc1e644
	github.com/go-filesystems/fat32 v0.3.0
	github.com/go-filesystems/interface v0.3.0
	github.com/go-filesystems/uefi v0.1.0
	github.com/go-tpm2/attest v0.3.0
	github.com/go-tpm2/common v0.1.0
	github.com/go-volumes/gpt v0.0.0-20260831115417-b3069a3ac03a
	github.com/go-volumes/safeio v0.0.0-20260831125406-d8f54b2890d4
)

require github.com/go-tpm2/tpm2 v0.6.0
