#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-2.0-only
"""Exercise upstream redundant MMC environments on compact sandbox cards."""
import pathlib
import struct
import subprocess
import sys
import tempfile
import zlib


def main():
    binary, dtc = (str(pathlib.Path(arg).resolve()) for arg in sys.argv[1:])
    with tempfile.TemporaryDirectory(prefix="mmc-test-", dir=".") as work:
        cwd = pathlib.Path(work).resolve()
        dtb = cwd / "control.dtb"
        subprocess.run([dtc, "-I", "dts", "-O", "dtb", "-o", dtb, "-"],
                       input=b'''/dts-v1/;
/ {
    binman {};
    aliases { mmc0 = &mmc; };
    config {
        u-boot,mmc-env-offset = <0x100000>;
        u-boot,mmc-env-offset-redundant = <0x200000>;
    };
    mmc: mmc { compatible = "sandbox,mmc"; filename = "disk.img"; };
};''', check=True)
        card = cwd / "disk.img"
        with card.open("wb") as f:
            f.truncate(4 * 1024 * 1024)

        def run(command, marker):
            result = subprocess.run([binary, "-d", dtb, "-c", command], cwd=cwd,
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                    text=True, timeout=30)
            assert result.returncode == 0 and marker in result.stdout and "FAIL:" not in result.stdout, result.stdout

        def copies():
            with card.open("rb") as f:
                result = []
                for offset in (0x100000, 0x200000):
                    f.seek(offset)
                    data = f.read(0x10000)
                    result.append(data)
                return result

        def valid(data):
            return struct.unpack("<I", data[:4])[0] == zlib.crc32(data[5:])

        run('setenv NABOS_RESET_SLOT A; setenv BOOT_ORDER "B A"; '
            'setenv BOOT_B_LEFT 3; '
            'if saveenv; then setenv NABOS_RESET_SLOT B; '
            'if env load && test "${NABOS_RESET_SLOT}" = A && '
            'test "${BOOT_ORDER}" = "B A" && test "${BOOT_B_LEFT}" = 3; '
            'then echo PASS: roundtrip; else echo FAIL: load; fi; '
            'else echo FAIL: save; fi', 'PASS: roundtrip')
        run('if test "${NABOS_RESET_SLOT}" = A; then echo PASS: restart; '
            'else echo FAIL: restart; fi; '
            'setenv marker second; saveenv', 'PASS: restart')
        before = copies()
        assert all(valid(data) for data in before), "both redundant copies must be valid"
        assert before[0] != before[1], "saveenv must alternate copies"
        run('nabos_env_test fail; '
            'if saveenv; then echo FAIL: write; else echo PASS: write; fi; '
            'nabos_env_test restore; '
            'if env load && test "${marker}" = second; then echo PASS: restored; '
            'else echo FAIL: restored; fi', 'PASS: restored')
        assert copies() == before, "failed write changed valid copies"
        latest = 0 if b"marker=second\0" in before[0] else 1
        with card.open("r+b") as f:
            f.seek((0x100000, 0x200000)[latest] + 100)
            f.write(bytes([before[latest][100] ^ 1]))
        run('if test "${NABOS_RESET_SLOT}" = A && test -z "${marker}"; '
            'then echo PASS: redundant; else echo FAIL: redundant; fi', 'PASS: redundant')
        with card.open("r+b") as f:
            f.seek((0x100000, 0x200000)[1 - latest])
            f.write(bytes(0x10000))
        run('if env load; then echo FAIL: crc; else echo PASS: crc; fi', 'PASS: crc')
    print("PASS: native redundant MMC roundtrip, restart, alternating copies, CRC fallback and write failures")


if __name__ == "__main__":
    main()
