#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-2.0-only
"""Exercise real sandbox saveenv/env load in isolated host-file fixtures."""
import pathlib
import subprocess
import sys
import tempfile


def main():
    binary, dtb = (str(pathlib.Path(arg).resolve()) for arg in sys.argv[1:])

    def run(cwd, command, marker):
        result = subprocess.run(
            [binary, "-d", dtb, "-c", command], cwd=cwd,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=30,
        )
        assert result.returncode == 0 and marker in result.stdout and "FAIL:" not in result.stdout, result.stdout

    with tempfile.TemporaryDirectory(prefix="env-test-", dir=".") as work:
        cwd = pathlib.Path(work).resolve()
        run(cwd, 'setenv NABOS_RESET_SLOT A; setenv BOOT_ORDER "B A"; '
            'setenv BOOT_B_LEFT 3; '
            'if saveenv; then setenv NABOS_RESET_SLOT B; setenv BOOT_ORDER "A B"; '
            'setenv BOOT_B_LEFT 0; '
            'if env load && test "${NABOS_RESET_SLOT}" = A && '
            'test "${BOOT_ORDER}" = "B A" && test "${BOOT_B_LEFT}" = 3; '
            'then echo PASS: roundtrip; else echo FAIL: load; fi; '
            'else echo FAIL: save; fi', 'PASS: roundtrip')
        file = cwd / "uboot.env"
        saved = file.read_bytes()
        assert len(saved) == 0x10000
        run(cwd, 'if test "${NABOS_RESET_SLOT}" = A; then echo PASS: restart; '
            'else echo FAIL: restart; fi', 'PASS: restart')
        with tempfile.TemporaryDirectory(prefix="other-", dir=cwd) as other:
            run(other, 'if test -z "${NABOS_RESET_SLOT}"; then echo PASS: isolation; '
                'else echo FAIL: isolation; fi', 'PASS: isolation')
        file.write_bytes(saved[:8])
        run(cwd, 'if env load; then echo FAIL: truncated; '
            'else echo PASS: truncated; fi', 'PASS: truncated')
        corrupt = bytearray(saved)
        corrupt[100] ^= 1
        file.write_bytes(corrupt)
        run(cwd, 'if env load; then echo FAIL: crc; '
            'else echo PASS: crc; fi', 'PASS: crc')
        file.unlink()
        run(cwd, 'if env load; then echo FAIL: missing; '
            'else echo PASS: missing; fi', 'PASS: missing')
        file.mkdir()
        run(cwd, 'if saveenv; then echo FAIL: write; '
            'else echo PASS: write; fi', 'PASS: write')
    print("PASS: sandbox host-file saveenv/env load roundtrip, restart, cwd isolation, truncated/CRC/read/write failures")


if __name__ == "__main__":
    main()
