# Component builds use the native Go, Cargo and U-Boot build systems.
SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.ONESHELL:
.DEFAULT_GOAL := help
# Do not pass NabOS command-line variables (notably VERSION) to U-Boot's make.
unexport MAKEFLAGS MFLAGS MAKEOVERRIDES MAKEFILES

TARGET ?= zero-armv6
VERSION ?= dev-local
INPUTS ?=
GO ?= go
export TARGET VERSION INPUTS GO

go package-go: export GOTOOLCHAIN = local
go package-go: export OUT ?= $(CURDIR)/build/go/$(TARGET)
rust package-rust: export OUT ?= $(CURDIR)/build/rust/$(TARGET)
device-core package-device-core: export OUT ?= $(CURDIR)/build/device-core-build/$(TARGET)
device-core-source: export OUT ?= $(CURDIR)/build/device-core-source
rust package-rust: export RUST_COMPONENT = nab-hardware
device-core package-device-core: export RUST_COMPONENT = device-core
uboot package-uboot: export OUT ?= $(CURDIR)/build/uboot/$(TARGET)

.PHONY: help go rust device-core device-core-source uboot package-go package-rust package-device-core package-uboot
help:
	@echo 'make {go,rust,device-core,uboot} TARGET={zero-armv6,zero2-arm64} [VERSION=dev-local]'
	echo 'make package-{go,rust,device-core,uboot} adds build/components/<component>-<target>.tar'
	echo 'Optional: OUT=<output-directory> INPUTS=<archived-replay-inputs>'

# Always invoke the language build systems; they own dependency tracking/caching.

go:
	repo=$$PWD
	target=$${TARGET:?TARGET required}
	version=$${VERSION:?VERSION required}
	out=$$(realpath -m "$$OUT")
	inputs=$${INPUTS:-}
	[[ $$target == zero-armv6 || $$target == zero2-arm64 ]] || exit 2
	[[ $$version =~ ^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$$ ]] || exit 2
	lock=$$repo/image/sources.lock.json
	[[ $$($$GO env GOVERSION) == "go$$(jq -r .tools.go "$$lock")" ]] || { echo 'Go toolchain mismatch' >&2; exit 1; }
	mkdir -p "$$out/inputs/go-modcache/cache"
	if [[ -n $$inputs ]]; then
	  inputs=$$(realpath "$$inputs")
	  cmp "$$repo/services/go.sum" "$$inputs/go.sum"
	  export GOMODCACHE="$$inputs/go-modcache" GOPROXY=off
	fi
	cd "$$repo/services"
	$$GO mod download
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(jq -r --arg t "$$target" '.targets[$$t].goarch' "$$lock") \
	  GOARM=$$(jq -r --arg t "$$target" '.targets[$$t].goarm' "$$lock") \
	  $$GO build -trimpath -ldflags="-s -w -X main.version=$$version" -o "$$out/nabos" ./cmd/nabos
	# Keep setup-go's module cache in place; archive the inputs needed for replay.
	cp -a "$$($$GO env GOMODCACHE)/cache/download" "$$out/inputs/go-modcache/cache/"
	cp go.sum "$$out/inputs/go.sum"
	printf '%s\n' "$$target" "$$(git rev-parse HEAD)" > "$$out/build-info"
	printf '%s\n' "$$version" > "$$out/version"

# Fetch only the pinned external archive; never compile the worker checkout.
device-core-source:
	repo=$$PWD
	out=$$(realpath -m "$$OUT")
	inputs=$${INPUTS:-}
	lock=$$repo/image/sources.lock.json
	mkdir -p "$$repo/build/iot" "$$out/inputs/device-core"
	(cd "$$repo/services" && GOTOOLCHAIN=local CGO_ENABLED=0 $$GO build -o "$$repo/build/iot/nab-image" ./cmd/nab-image)
	if [[ -n $$inputs ]]; then
	  inputs=$$(realpath "$$inputs")
	  cp "$$inputs/device-core/device_core.tar.gz" "$$out/inputs/device-core/"
	  digest=$$(jq -er .sources.device_core.sha256 "$$lock")
	  printf '%s  %s\n' "$$digest" "$$out/inputs/device-core/device_core.tar.gz" | sha256sum --check --strict
	  jq -S .sources.device_core "$$lock" | cmp - "$$inputs/device-core/source-identity.json"
	fi
	rm -rf "$$out/.source"
	"$$repo/build/iot/nab-image" source "$$lock" device_core "$$out/inputs/device-core" "$$out/.source" > "$$out/source-path"
	jq -S .sources.device_core "$$lock" > "$$out/inputs/device-core/source-identity.json"

rust device-core:
	repo=$$PWD
	target=$${TARGET:?TARGET required}
	out=$$(realpath -m "$$OUT")
	inputs=$${INPUTS:-}
	component=$$RUST_COMPONENT
	source=$$repo/core
	if [[ $$component == device-core ]]; then
	  make -C "$$repo" device-core-source OUT="$$out" INPUTS="$$inputs"
	  source=$$(cat "$$out/source-path")
	fi
	component_inputs=$$out/inputs/$$component
	case $$target in
	  zero-armv6) triple=arm-linux-gnueabihf; cpu=(-mcpu=arm1176jzf-s -mfpu=vfp -mfloat-abi=hard) ;;
	  zero2-arm64) triple=aarch64-linux-gnu; cpu=(-mcpu=cortex-a53) ;;
	  *) exit 2 ;;
	esac
	lock=$$repo/image/sources.lock.json
	[[ $$(rustc --version | cut -d' ' -f2) == "$$(jq -r .tools.rust "$$lock")" ]] || { echo 'Rust toolchain mismatch' >&2; exit 1; }
	rust_target=$$(jq -r --arg t "$$target" '.targets[$$t].rust_target' "$$lock")
	mkdir -p "$$out/inputs/rust-sysroot" "$$component_inputs"
	if [[ -n $$inputs ]]; then
	  inputs=$$(realpath "$$inputs")
	  cmp "$$source/Cargo.lock" "$$inputs/$$component/Cargo.lock"
	  cmp "$$repo/image/rust-sysroots.lock.json" "$$inputs/rust-sysroots.lock.json"
	  cp -a "$$inputs/rust-sysroot/." "$$out/inputs/rust-sysroot/"
	  rm -rf "$$component_inputs/cargo-vendor"
	  cp -a "$$inputs/$$component/cargo-vendor" "$$component_inputs/"
	else
	  cargo vendor --locked --manifest-path "$$source/Cargo.toml" "$$component_inputs/cargo-vendor" > /dev/null
	fi
	sysroot=$$repo/build/sysroot/$$component/$$target
	rm -rf "$$sysroot"
	mkdir -p "$$sysroot/usr/lib"
	ln -s usr/lib "$$sysroot/lib"
	while IFS=$$'\t' read -r name url hash; do
	  deb=$$out/inputs/rust-sysroot/$$name.deb
	  if ! printf '%s  %s\n' "$$hash" "$$deb" | sha256sum --check --status; then
	    [[ -z $$inputs ]] || { echo "Missing or corrupt replay sysroot package: $$name" >&2; exit 1; }
	    curl --fail --location --retry 3 "$$url" -o "$$deb.part"
	    printf '%s  %s\n' "$$hash" "$$deb.part" | sha256sum --check
	    mv "$$deb.part" "$$deb"
	  fi
	  dpkg-deb --extract "$$deb" "$$sysroot"
	done < <(jq -r --arg t "$$target" '.[$$t] | to_entries[] | [.key, .value.url, .value.sha256] | @tsv' "$$repo/image/rust-sysroots.lock.json")
	# dpkg packages can contain absolute links; keep all lookup inside the sysroot.
	while IFS= read -r -d '' link; do
	  destination=$$(readlink "$$link")
	  ln -sfn "$$(realpath -m --relative-to="$$(dirname "$$link")" "$$sysroot$$destination")" "$$link"
	done < <(find "$$sysroot" -type l -lname '/*' -print0)
	linker=$$sysroot/target-cc-$$(sha256sum "$$repo/image/rust-sysroots.lock.json" | cut -c1-16)
	printf '#!/bin/bash\nexec %s"$$@"\n' "$$(printf '%q ' clang "--target=$$triple" "--sysroot=$$sysroot" "--gcc-toolchain=$$sysroot/usr" -fuse-ld=lld -Wno-unused-command-line-argument "$${cpu[@]}")" > "$$linker"
	chmod 755 "$$linker"
	cc_key=CC_$${rust_target//-/_}
	ar_key=AR_$${rust_target//-/_}
	linker_key=CARGO_TARGET_$$(tr '[:lower:]-' '[:upper:]_' <<< "$$rust_target")_LINKER
	export CARGO_TARGET_DIR=$${CARGO_TARGET_DIR:-$$repo/build/cargo/$$component}
	# ring and other cc-rs builds need the same target CPU, headers and libc.
	env "$$cc_key=$$linker" "$$ar_key=llvm-ar" "$$linker_key=$$linker" cargo --config 'source.crates-io.replace-with="vendored-sources"' \
	  --config "source.vendored-sources.directory=\"$$component_inputs/cargo-vendor\"" \
	  build --locked --offline --release --manifest-path "$$source/Cargo.toml" --target "$$rust_target"
	install -m755 "$$CARGO_TARGET_DIR/$$rust_target/release/$$component" "$$out/$$component"
	cp "$$source/Cargo.lock" "$$component_inputs/"
	if [[ $$component == nab-hardware ]]; then git -C "$$repo" rev-parse HEAD > "$$component_inputs/source-revision"; fi
	cp "$$repo/image/rust-sysroots.lock.json" "$$out/inputs/"
	printf '%s\n' "$$target" "$$(git -C "$$repo" rev-parse HEAD)" > "$$out/build-info"
	if [[ $$component == device-core ]]; then
	  jq -r .sources.device_core.commit "$$lock" >> "$$out/build-info"
	  sha256sum "$$out/device-core" | cut -d' ' -f1 > "$$out/binary.sha256"
	  "$$repo/build/iot/nab-image" verify-device-core "$$lock" "$$out" "$$target" "$$(git -C "$$repo" rev-parse HEAD)"
	fi

uboot:
	export LC_ALL=C
	repo=$$PWD
	die() { echo "uboot: $$*" >&2; exit 1; }
	target=$${TARGET:?TARGET required}
	case $$target in
	  zero-armv6) triple=arm-linux-gnueabihf; machine=ARM ;;
	  zero2-arm64) triple=aarch64-linux-gnu; machine=AArch64 ;;
	  *) die "Unknown target: $$target" ;;
	esac
	host=$$(uname -m)
	[[ $$host == x86_64 || $$host == aarch64 ]] || die 'An x86_64 or ARM64 Ubuntu host is required'
	for tool in git jq curl tar make gcc bison flex bc python3 dpkg-deb sha256sum; do
	  command -v "$$tool" >/dev/null || die "Missing host tool: $$tool"
	done
	replay=
	if [[ -n $$INPUTS ]]; then
	  replay=$$(realpath "$$INPUTS")
	  [[ -d $$replay && -f $$replay/uboot.tar.gz ]] || die 'Replay requires INPUTS_DIR/uboot.tar.gz'
	fi
	mkdir -p "$$OUT"
	out=$$(realpath "$$OUT")
	work=$$out/.build
	# A stable path lets ccache reuse objects. Refuse concurrent builds here.
	mkdir "$$work" || die "Build directory already exists: $$work"
	trap 'rm -rf "$$work"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	rm -f "$$out/u-boot.bin" "$$out/build-info"
	inputs=$$work/inputs
	toolchain=$$inputs/uboot-toolchain
	mkdir -p "$$inputs" "$$work/src" "$$work/obj" "$$work/toolchain"
	lock=$$repo/image/sources.lock.json
	url=$$(jq -er '.sources.uboot.url' "$$lock")
	digest=$$(jq -er '.sources.uboot.sha256' "$$lock")
	defconfig=$$(jq -er --arg target "$$target" '.targets[$$target].uboot_defconfig' "$$lock")
	[[ $$url == https://* && $$digest =~ ^[0-9a-f]{64}$$ && $$defconfig =~ ^[a-zA-Z0-9_]+_defconfig$$ ]] || die 'Invalid U-Boot source lock'
	if [[ -n $$replay ]]; then
	  cp "$$replay/uboot.tar.gz" "$$inputs/"
	  if [[ -f $$replay/uboot.fragment.config ]]; then
	    cmp "$$repo/image/boot/uboot.config" "$$replay/uboot.fragment.config"
	  fi
	else
	  curl --fail --location --retry 3 --proto '=https' --proto-redir '=https' \
	    "$$url" -o "$$inputs/uboot.tar.gz"
	fi
	printf '%s  %s\n' "$$digest" "$$inputs/uboot.tar.gz" | sha256sum --check --strict
	cp "$$repo/image/boot/uboot.config" "$$inputs/uboot.fragment.config"

	if [[ -n $$replay ]]; then
	  [[ -d $$replay/uboot-toolchain ]] || die 'Replay requires the archived U-Boot toolchain'
	  cp -a "$$replay/uboot-toolchain" "$$toolchain"
	else
	  # Capture the packages that own GCC, cc1, libgcc/headers and binutils.
	  # This avoids both relying on tomorrow's APT versions and shipping a sysroot:
	  # U-Boot is freestanding, and ARMv6 uses its own libgcc below.
	  command -v "$$triple-gcc" >/dev/null || die "Install gcc-$$triple before building"
	  compiler=$$(readlink -f "$$(command -v "$$triple-gcc")")
	  mkdir "$$toolchain"
	  packages=()
	  files=("$$compiler" "$$("$$compiler" -print-prog-name=cc1)" \
	    "$$("$$compiler" -print-libgcc-file-name)" \
	    "$$(command -v "$$triple-as")" "$$(command -v "$$triple-ld")")
	  # Native ARM64 binutils may put these versioned libraries in libbinutils,
	  # whereas the x86 cross packages include them directly.
	  while IFS= read -r library; do files+=("$$library"); done < <(
	    ldd "$$(command -v "$$triple-as")" "$$(command -v "$$triple-ld")" |
	      awk '/lib(bfd|ctf|opcodes).* => \// { print $$3 }' | sort -u)
	  for file in "$${files[@]}"; do
	    owner=$$(dpkg-query --search "$$(readlink -f "$$file")")
	    packages+=("$${owner%%: /*}")
	  done
	  dpkg-query -W -f='$${binary:Package}\t$${Version}\t$${Architecture}\n' "$${packages[@]}" |
	    sort -u > "$$toolchain/manifest.tsv"
	  while IFS=$$'\t' read -r package version _; do
	    (cd "$$toolchain" && apt-get download "$$package=$$version")
	  done < "$$toolchain/manifest.tsv"
	  basename "$$compiler" > "$$toolchain/compiler"
	  printf '%s\n' "$$target" > "$$toolchain/target"
	  printf '%s\n' "$$host" > "$$toolchain/host"
	  (cd "$$toolchain" && sha256sum ./*.deb compiler target host manifest.tsv > SHA256SUMS)
	fi
	# Hash both the package set and its metadata; extra unlisted .debs are rejected.
	(cd "$$toolchain" && sha256sum --check --strict SHA256SUMS
	  sha256sum ./*.deb compiler target host manifest.tsv > "$$work/toolchain.SHA256SUMS")
	cmp "$$toolchain/SHA256SUMS" "$$work/toolchain.SHA256SUMS"
	[[ $$(cat "$$toolchain/target") == "$$target" ]] || die 'Replay toolchain target mismatch'
	[[ $$(cat "$$toolchain/host") == "$$host" ]] || die 'Replay requires the toolchain original host architecture'
	compiler=$$(cat "$$toolchain/compiler")
	[[ $$compiler =~ ^($$triple-)?gcc-[0-9]+$$ ]] || die 'Invalid archived compiler name'
	for package in "$$toolchain"/*.deb; do dpkg-deb --extract "$$package" "$$work/toolchain"; done
	cross=$$work/toolchain/usr/bin/$$triple-
	[[ -x $$work/toolchain/usr/bin/$$compiler ]] || die 'Archived GCC is missing'
	ln -s "$$compiler" "$${cross}gcc"
	# Cross-binutils carries libbfd/libctf alongside its executables. GCC discovers
	# cc1, headers and libgcc relative to the extracted /usr tree without sudo.
	host_triple=$$(gcc -dumpmachine)
	export LD_LIBRARY_PATH="$$work/toolchain/usr/lib/$$host_triple$${LD_LIBRARY_PATH:+:$$LD_LIBRARY_PATH}"
	[[ $$("$${cross}gcc" -dumpmachine) == "$$triple" ]] || die 'Cross-compiler target mismatch'
	# Extract only after authenticating both the sources and archived toolchain.
	tar --extract --gzip --file="$$inputs/uboot.tar.gz" --directory="$$work/src" \
	  --strip-components=1 --no-same-owner
	cc="$${cross}gcc"
	if command -v ccache >/dev/null; then
	  export CCACHE_DIR=$${CCACHE_DIR:-$$repo/build/cache/uboot/$$target}
	  export CCACHE_BASEDIR=$$work CCACHE_COMPILERCHECK=content
	  CCACHE_NAMESPACE=$$(sha256sum "$$toolchain/SHA256SUMS" "$$inputs/uboot.tar.gz" \
	    "$$inputs/uboot.fragment.config" "$$repo/Makefile" | sha256sum | cut -d' ' -f1)
	  export CCACHE_NAMESPACE
	  cc="ccache $$cc"
	fi
	# The locked archive's timestamp is stable, including when replayed elsewhere.
	export SOURCE_DATE_EPOCH
	SOURCE_DATE_EPOCH=$$(stat -c %Y "$$work/src/Makefile")
	export KBUILD_BUILD_USER=nabos KBUILD_BUILD_HOST=builder
	build=(make -C "$$work/src" O="$$work/obj" CROSS_COMPILE="$$cross" CC="$$cc")
	"$${build[@]}" "$$defconfig"
	"$$work/src/scripts/kconfig/merge_config.sh" -m -O "$$work/obj" \
	  "$$work/obj/.config" "$$inputs/uboot.fragment.config"
	if [[ $$target == zero-armv6 ]]; then
	  "$$work/src/scripts/config" --file "$$work/obj/.config" -e USE_PRIVATE_LIBGCC
	fi
	"$${build[@]}" olddefconfig
	# shellcheck disable=SC2016 # U-Boot expands scriptaddr at boot, not this shell.
	for option in CONFIG_ENV_IS_IN_MMC=y CONFIG_ENV_REDUNDANT=y CONFIG_ENV_SIZE=0x10000 \
	  CONFIG_ENV_OFFSET=0x100000 CONFIG_ENV_OFFSET_REDUND=0x200000 CONFIG_ENV_MMC_DEVICE_INDEX=0 \
	  '# CONFIG_ENV_IS_IN_FAT is not set' CONFIG_OF_LIBFDT_OVERLAY=y CONFIG_CMD_SETEXPR=y \
	  CONFIG_WDT=y CONFIG_WDT_BCM2835=y CONFIG_CMD_WDT=y CONFIG_WATCHDOG=y \
	  '# CONFIG_WATCHDOG_AUTOSTART is not set' CONFIG_BOOTDELAY=-2 CONFIG_USE_BOOTCOMMAND=y \
	  'CONFIG_BOOTCOMMAND="load mmc 0:1 $${scriptaddr} boot.scr && source $${scriptaddr}"'; do
	  grep -qxF "$$option" "$$work/obj/.config" || die "U-Boot lacks $$option"
	done
	if [[ $$target == zero-armv6 ]]; then
	  for option in CONFIG_CPU_ARM1176=y CONFIG_USE_PRIVATE_LIBGCC=y; do
	    grep -qxF "$$option" "$$work/obj/.config" || die "U-Boot lacks $$option"
	  done
	fi
	"$${build[@]}" -j"$$(nproc)"
	# Inspect the just-linked ELF even on a warm cache. The parent keeps the actual
	# sandbox boot-script tests; these checks do not claim to test a hardware boot.
	"$${cross}readelf" -h -A -l -d "$$work/obj/u-boot" > "$$inputs/uboot.elf.txt"
	grep -Eq "Machine: +$$machine$$" "$$inputs/uboot.elf.txt" || die 'Wrong U-Boot ELF architecture'
	if grep -Eq 'INTERP|\(NEEDED\)' "$$inputs/uboot.elf.txt"; then die 'U-Boot must be freestanding'; fi
	if [[ $$target == zero-armv6 ]]; then
	  grep -Eq 'Tag_CPU_arch: v[4-6]([^0-9]|$$)' "$$inputs/uboot.elf.txt" || die 'Missing ARMv6-compatible ELF attributes'
	  if grep -Eq 'Tag_CPU_arch: v([7-9]|[1-9][0-9])|Tag_THUMB_ISA_use: Thumb-2' "$$inputs/uboot.elf.txt"; then
	    die 'U-Boot requires instructions newer than ARMv6'
	  fi
	  if grep -Eq 'libgcc\.a|libc\.a' "$$work/obj/u-boot.map"; then die 'ARMv6 linked an external runtime'; fi
	fi
	test -s "$$work/obj/u-boot.bin"
	cp "$$work/obj/.config" "$$inputs/uboot.config"
	{
	  printf 'target=%s\nsource_sha256=%s\nsource_date_epoch=%s\n' "$$target" "$$digest" "$$SOURCE_DATE_EPOCH"
	  "$${cross}gcc" --version
	  "$${cross}ld" --version
	  (cd "$$work/obj" && sha256sum u-boot.bin)
	} > "$$inputs/uboot-build.txt"
	rm -rf "$$out/inputs"
	mv "$$inputs" "$$out/inputs"
	install -m644 "$$work/obj/u-boot.bin" "$$out/u-boot.bin"
	printf '%s\n%s\n' "$$target" "$$(git -C "$$repo" rev-parse HEAD)" > "$$out/build-info"

package-go: go
package-rust: rust
package-device-core: device-core
package-uboot: uboot
package-go package-rust package-device-core package-uboot:
	mkdir -p build/components
	tar -C "$$OUT" --exclude=./.source --exclude=./source-path -cf "build/components/$(@:package-%=%)-$$TARGET.tar" .
