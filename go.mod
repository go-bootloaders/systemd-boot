module github.com/go-bootloaders/systemd-boot

go 1.26.4

require (
	github.com/go-filesystems/detect v0.0.0-20260622114748-623b36231685
	github.com/go-filesystems/detect/fat32reg v0.0.0-20260805205105-f4d6b850ab1e
	github.com/go-filesystems/fat32 v0.0.0-20260622110031-1d68bab25618
	github.com/go-filesystems/interface v0.0.0-20260622072638-0b01d4fb163f
	github.com/go-filesystems/uefi v0.0.0-20260622110039-8bad894e467d
	github.com/go-tpm2/attest v0.3.0
	github.com/go-tpm2/common v0.1.0
	github.com/go-volumes/gpt v0.0.0-20260806060839-7211933d4c65
	github.com/go-volumes/safeio v0.0.0-20260622072324-7f8eb19f6f8c
)

require github.com/go-tpm2/tpm2 v0.6.0
