# Nix owns component dependencies, cross toolchains and image construction.
SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
TARGET ?= zero-armv6
VERSION ?= dev-local
DEVELOPMENT ?= 0
PHASE ?= cold

.PHONY: help image check benchmark
help:
	@echo 'make image TARGET={zero-armv6,zero2-arm64} VERSION=dev-local DEVELOPMENT=1'
	@echo 'Signed image: export RAUC_KEY and RAUC_CERT paths, then make image VERSION=vX.Y.Z'
	@echo 'make check; make benchmark TARGET=... PHASE={cold,warm,version,nixpkgs}'

image:
	bash image/build.sh "$(TARGET)" "$(VERSION)" $(if $(filter 1,$(DEVELOPMENT)),--development)

check:
	nix --extra-experimental-features 'nix-command flakes' flake check --no-build --option allow-import-from-derivation false
	python3 image/cache-report.py --self-test
	for script in image/*.sh nix/runtime/*.sh; do bash -n "$$script"; done

benchmark:
	bash image/benchmark.sh "$(PHASE)" "$(TARGET)" "$(VERSION)"
