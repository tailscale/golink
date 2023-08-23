module github.com/tailscale/golink

go 1.21

toolchain go1.21.0

require (
	github.com/google/go-cmp v0.5.9
	modernc.org/sqlite v1.19.4
	// Always use a pseudo-version for the tailscale.com module, or else
	// go's version selection causes problems when pulling golink into corp.
	tailscale.com v1.48.1
)

require golang.org/x/net v0.10.0

require (
	filippo.io/edwards25519 v1.0.0 // indirect
	github.com/Microsoft/go-winio v0.6.1 // indirect
	github.com/akutz/memconn v0.1.0 // indirect
	github.com/alexbrainman/sspi v0.0.0-20210105120005-909beea2cc74 // indirect
	github.com/aws/aws-sdk-go-v2 v1.18.0 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.18.22 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.13.21 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.13.3 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.1.33 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.4.27 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.3.34 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.9.27 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssm v1.36.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.12.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.14.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.18.10 // indirect
	github.com/aws/smithy-go v1.13.5 // indirect
	github.com/coreos/go-iptables v0.6.0 // indirect
	github.com/dblohm7/wingoes v0.0.0-20230803162905-5c6286bb8c6e // indirect
	github.com/fxamacker/cbor/v2 v2.4.0 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/nftables v0.1.1-0.20230115205135-9aa6fdf5a28c // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/hdevalence/ed25519consensus v0.1.0 // indirect
	github.com/illarion/gonotify v1.0.1 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20230407062729-974c6f05fe16 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/josharian/native v1.1.1-0.20230202152459-5c7d0dd6ab86 // indirect
	github.com/jsimonetti/rtnetlink v1.3.2 // indirect
	github.com/kballard/go-shellquote v0.0.0-20180428030007-95032a82bc51 // indirect
	github.com/klauspost/compress v1.16.7 // indirect
	github.com/kortschak/wol v0.0.0-20200729010619-da482cc4850a // indirect
	github.com/mattn/go-isatty v0.0.18 // indirect
	github.com/mdlayher/genetlink v1.3.2 // indirect
	github.com/mdlayher/netlink v1.7.2 // indirect
	github.com/mdlayher/sdnotify v1.0.0 // indirect
	github.com/mdlayher/socket v0.4.1 // indirect
	github.com/miekg/dns v1.1.55 // indirect
	github.com/mitchellh/go-ps v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.17 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20200410134404-eec4a21b6bb0 // indirect
	github.com/tailscale/certstore v0.1.1-0.20220316223106-78d6e1c49d8d // indirect
	github.com/tailscale/golang-x-crypto v0.0.0-20230713185742-f0b76a10a08e // indirect
	github.com/tailscale/goupnp v1.0.1-0.20210804011211-c64d0f06ea05 // indirect
	github.com/tailscale/netlink v1.1.1-0.20211101221916-cabfb018fe85 // indirect
	github.com/tailscale/wireguard-go v0.0.0-20230710185534-bb2c8f22eccf // indirect
	github.com/tcnksm/go-httpstat v0.2.0 // indirect
	github.com/u-root/uio v0.0.0-20230305220412-3e8cd9d6bf63 // indirect
	github.com/vishvananda/netlink v1.2.1-beta.2 // indirect
	github.com/vishvananda/netns v0.0.4 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go4.org/mem v0.0.0-20220726221520-4f986261bf13 // indirect
	go4.org/netipx v0.0.0-20230728180743-ad4cb58a6516 // indirect
	golang.org/x/crypto v0.11.0 // indirect
	golang.org/x/exp v0.0.0-20230725093048-515e97ebf090 // indirect
	golang.org/x/mod v0.11.0 // indirect
	golang.org/x/sync v0.2.0 // indirect
	golang.org/x/sys v0.10.0 // indirect
	golang.org/x/term v0.10.0 // indirect
	golang.org/x/text v0.11.0 // indirect
	golang.org/x/time v0.3.0 // indirect
	golang.org/x/tools v0.9.1 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	golang.zx2c4.com/wireguard/windows v0.5.3 // indirect
	gvisor.dev/gvisor v0.0.0-20230504175454-7b0a1988a28f // indirect
	inet.af/peercred v0.0.0-20210906144145-0893ea02156a // indirect
	lukechampine.com/uint128 v1.2.0 // indirect
	modernc.org/cc/v3 v3.40.0 // indirect
	modernc.org/ccgo/v3 v3.16.13 // indirect
	modernc.org/libc v1.21.4 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.4.0 // indirect
	modernc.org/opt v0.1.3 // indirect
	modernc.org/strutil v1.1.3 // indirect
	modernc.org/token v1.0.1 // indirect
	nhooyr.io/websocket v1.8.7 // indirect
)
