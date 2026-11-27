module github.com/go-bootloaders/systemd-boot

go 1.27.4

require (
	github.com/go-filesystems/detect v0.6.0
	github.com/go-filesystems/detect/fat32reg v0.0.0-20261127003434-b4155692a411
	github.com/go-filesystems/fat32 v0.13.1
	github.com/go-filesystems/interface v0.6.0
	github.com/go-filesystems/uefi v0.5.0
	github.com/go-tpm2/attest v0.5.0
	github.com/go-tpm2/common v0.3.0
	github.com/go-tpm2/tpm2 v0.12.0
	github.com/go-volumes/gpt v0.3.0
	github.com/go-volumes/safeio v0.1.1
)

require github.com/go-authn/cms v0.2.0 // indirect
