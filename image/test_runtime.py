#!/usr/bin/env python3
"""Runtime checks for the NabOS OS image files (image/boot, image/rootfs, genimage.cfg).

Run: python3 -m unittest discover -s image -p 'test_*.py'
Nothing here touches host block devices or needs root. The U-Boot tests run
the real boot.scr in a sandbox build of the pinned U-Boot and are skipped when
NABOS_UBOOT_SANDBOX (default /tmp/nabos-uboot-sandbox) has no u-boot binary:
  make O=$NABOS_UBOOT_SANDBOX sandbox_defconfig
  scripts/config --file $NABOS_UBOOT_SANDBOX/.config -d SANDBOX_SDL -d TOOLS_MKEFICAPSULE \
      -d UNIT_TEST -d EFI_CAPSULE_AUTHENTICATE -d EFI_CAPSULE_ON_DISK -d CMD_UPL -d UPL
  make O=$NABOS_UBOOT_SANDBOX olddefconfig
  make O=$NABOS_UBOOT_SANDBOX CONFIG_PYLIBFDT= u-boot tools
"""

import os
import re
import shutil
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path

IMAGE = Path(__file__).resolve().parent
BOOT = IMAGE / "boot"
ROOTFS = IMAGE / "rootfs"
SANDBOX = Path(os.environ.get("NABOS_UBOOT_SANDBOX", "/tmp/nabos-uboot-sandbox"))
SOURCES = Path(os.environ.get("NABOS_SOURCES", "/tmp/nabos-sources"))
# Vendor DTBs from the pinned Raspberry Pi OS images (boot FAT); synthetic base otherwise.
VENDOR_DTBS = Path(os.environ.get("NABOS_VENDOR_DTBS", "/tmp/nabos-vendor-dtb"))
DTBS = {"zero-armv6": "bcm2708-rpi-zero-w.dtb", "zero2-arm64": "bcm2710-rpi-zero-2-w.dtb"}
MiB = 1024 * 1024


def run(*args, **kwargs):
    kwargs.setdefault("check", True)
    kwargs.setdefault("capture_output", True)
    kwargs.setdefault("text", True)
    return subprocess.run([str(a) for a in args], **kwargs)


def uboot_env():
    return dict(
        line.split("=", 1)
        for line in (BOOT / "uboot.env").read_text().splitlines()
        if line and not line.startswith("#")
    )


BASE_DTS = """/dts-v1/;
/ {
	compatible = "raspberrypi,model-zero-w", "brcm,bcm2835";
	#address-cells = <1>;
	#size-cells = <1>;
	soc {
		#address-cells = <1>;
		#size-cells = <1>;
		gpio: gpio@7e200000 { reg = <0x7e200000 0xb4>; gpio-controller; #gpio-cells = <2>; };
		i2s: i2s@7e203000 { reg = <0x7e203000 0x24>; #sound-dai-cells = <0>; status = "disabled"; };
		i2c1: i2c@7e804000 { reg = <0x7e804000 0x1000>; #address-cells = <1>; #size-cells = <0>; status = "disabled"; };
	};
	sound: sound { status = "disabled"; };
	vdd_5v0_reg: fixedregulator_5v0 { compatible = "regulator-fixed"; };
	vdd_3v3_reg: fixedregulator_3v3 { compatible = "regulator-fixed"; };
};
"""

MINI_OVERLAY = """/dts-v1/;
/plugin/;
/ { fragment@0 { target = <&%s>; __overlay__ { status = "okay"; }; }; };
"""


@unittest.skipUnless((SANDBOX / "u-boot").exists(), "U-Boot sandbox build not available")
class BootScript(unittest.TestCase):
    """boot.cmd under the pinned U-Boot's own hush parser, with a stored environment
    made only of uboot.env (env import -d wipes everything else, like a real
    stored environment does)."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = Path(tempfile.mkdtemp(prefix="nabos-boot-"))
        dtc = SANDBOX / "scripts/dtc/dtc"
        run(dtc, "-@", "-I", "dts", "-O", "dtb", "-o", cls.tmp / "base.dtb", "-", input=BASE_DTS)
        # Exercise the actual wdt commands using U-Boot's emulated driver,
        # named like the firmware's hardware device on both Raspberry Pis.
        run(dtc, "-I", "dts", "-O", "dtb", "-o", cls.tmp / "control.dtb", "-", input='''
/dts-v1/;
/ {
    binman {};
    reset { compatible = "sandbox,reset"; };
    watchdog@7e100000 { compatible = "sandbox,wdt"; };
};
''')
        cls.overlays = {}
        for name, repo in (("tagtagtag-sound", "sound"), ("tagtagtag-ears", "ears")):
            real = sorted((SOURCES / repo).rglob(f"{name}-overlay.dts"))
            if real:  # the pinned driver overlays, preprocessed like image/prepare.sh does
                dts = run("cpp", "-nostdinc", "-undef", "-D__DTS__", "-x", "assembler-with-cpp", "-P", real[0]).stdout
            else:
                dts = MINI_OVERLAY % ("i2s" if name == "tagtagtag-sound" else "gpio")
            out = cls.tmp / f"{name}.dtbo"
            run(dtc, "-@", "-I", "dts", "-O", "dtb", "-o", out, "-", input=dts)
            cls.overlays[name] = out
        run(SANDBOX / "tools/mkimage", "-A", "arm", "-T", "script", "-C", "none", "-d", BOOT / "boot.cmd", cls.tmp / "boot.scr")
        # Same packing as image/build.sh; comment lines must be dropped by mkenvimage.
        run(SANDBOX / "tools/mkenvimage", "-r", "-s", "0x10000", "-o", cls.tmp / "uboot.env.bin", BOOT / "uboot.env")

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp)

    def slot_tree(self, name, target, broken_overlay=False):
        tree = self.tmp / name
        shutil.rmtree(tree, ignore_errors=True)
        (tree / "boot/dtb").mkdir(parents=True)
        (tree / "boot/overlays").mkdir()
        (tree / "boot/kernel").write_bytes(b"not a real kernel" * 64)
        vendor = VENDOR_DTBS / DTBS[target]
        shutil.copy(vendor if vendor.exists() else self.tmp / "base.dtb", tree / "boot/dtb" / DTBS[target])
        for overlay, path in self.overlays.items():
            shutil.copy(path, tree / "boot/overlays" / f"{overlay}.dtbo")
        if broken_overlay:
            (tree / "boot/overlays/tagtagtag-ears.dtbo").write_bytes(b"garbage")
        return tree

    def disk(self, target, slot_a=True, slot_b=False, broken_overlay=False):
        """MBR disk shaped like genimage.cfg (smaller): p1 boot, p2 A, p3 B, p4 data."""
        boot = self.tmp / "p1"
        shutil.rmtree(boot, ignore_errors=True)
        boot.mkdir()
        shutil.copy(self.tmp / "boot.scr", boot / "boot.scr")
        shutil.copy(self.tmp / "uboot.env.bin", boot / "uboot.env")
        env = (BOOT / "boot.env.in").read_text().replace("@TARGET@", target).replace("@DTB@", DTBS[target])
        (boot / "boot.env").write_text(env)
        parts = [boot, self.slot_tree("a", target, broken_overlay) if slot_a else None,
                 self.slot_tree("b", target) if slot_b else None, None]
        size = 8 * MiB
        disk = self.tmp / "disk.img"
        with open(disk, "wb") as f:
            f.truncate(4 * MiB + 4 * size)
        table = "".join(f"start={(4 * MiB + i * size) // 512},size={size // 512},type={'c' if i == 0 else '83'}\n" for i in range(4))
        run("sfdisk", "-q", disk, input="label: dos\n" + table)
        for i, tree in enumerate(parts):
            if tree is None:
                continue
            part = self.tmp / f"part{i}.ext4"
            part.unlink(missing_ok=True)
            run("mkfs.ext4", "-q", "-F", "-d", tree, part, f"{size // 1024}k")
            with open(disk, "r+b") as f, open(part, "rb") as p:
                f.seek(4 * MiB + i * size)
                shutil.copyfileobj(p, f)
        return disk

    def boot(self, disk, env_changes="", watchdog=True):
        # env import -d -c: the CRC-checked redundant binary replaces the whole
        # environment, exactly like a valid stored environment on the card.
        commands = (
            f"host bind 0 {disk}; "
            "load host 0:1 0x100000 uboot.env && env import -d -c 0x100000 0x10000; "
            "setenv devtype host; setenv board_revision 0x9000C1; "
            + env_changes + " load host 0:1 ${scriptaddr} boot.scr; source ${scriptaddr}"
        )
        # The sandbox relaunches itself on "reset": stop at the second banner.
        control = ["-d", str(self.tmp / "control.dtb")] if watchdog else []
        proc = subprocess.Popen([str(SANDBOX / "u-boot"), *control, "-c", commands], stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, errors="replace")
        timer = threading.Timer(60, proc.kill)
        timer.start()
        output, banners = [], 0
        for line in proc.stdout:
            if line.startswith("U-Boot 20"):
                banners += 1
                if banners == 2:
                    output.append("<reset>")
                    break
            output.append(line.rstrip("\n"))
        proc.kill()
        proc.wait()
        proc.stdout.close()
        timer.cancel()
        text = "\n".join(output)
        self.assertNotIn("syntax error", text)
        self.assertNotIn("Unknown command", text)
        self.assertNotIn("Stopping watchdog timer failed", text)
        return [line.strip() for line in output if line.strip().startswith("nabos:")], text

    def test_stored_environment_is_complete(self):
        _, text = self.boot(self.disk("zero-armv6"), "printenv bootcmd bootdelay scriptaddr;")
        self.assertIn("bootcmd=load mmc 0:1 ${scriptaddr} boot.scr && source ${scriptaddr}", text)
        self.assertIn("bootdelay=-2", text)
        self.assertIn("scriptaddr=0x05400000", text)

    def test_first_boot_uses_slot_a_and_never_tries_empty_b(self):
        lines, output = self.boot(self.disk("zero-armv6"))
        self.assertEqual(lines[0], "nabos: trying slot A, 2 attempts left after this one")
        self.assertIn("nabos: overlay tagtagtag-sound applied", lines)
        self.assertIn("nabos: overlay tagtagtag-ears applied", lines)
        booting = next(line for line in lines if line.startswith("nabos: booting slot A:"))
        for arg in ("root=/dev/mmcblk0p2", "rauc.slot=A", "ro", "init=/usr/lib/nabos/boot-init",
                    "watchdog.open_timeout=300", "panic=10"):
            self.assertIn(arg, booting.split())
        self.assertIn("Started watchdog@7e100000", output)
        self.assertIn("nabos: board revision 0x009000c1", [line.lower() for line in lines])
        self.assertIn("Unrecognized zImage", output)  # bootz was used for ARMv6
        # Only reached in the sandbox because bootz returned.
        self.assertEqual(lines[-3:], ["nabos: slot A did not boot", "nabos: slot B has no attempts left",
                                      "nabos: no bootable slot, restoring boot attempts"])

    def test_primary_b_without_kernel_falls_through_to_a_in_the_same_boot(self):
        lines, _ = self.boot(self.disk("zero-armv6"), "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 3;")
        self.assertEqual(lines[:3], ["nabos: trying slot B, 2 attempts left after this one",
                                     "nabos: slot B did not boot",
                                     "nabos: trying slot A, 2 attempts left after this one"])
        self.assertTrue(any(line.startswith("nabos: booting slot A:") and "root=/dev/mmcblk0p2" in line for line in lines))

    def test_slot_b_boots_from_partition_3(self):
        lines, output = self.boot(self.disk("zero-armv6", slot_b=True), "setenv BOOT_ORDER 'B A'; setenv BOOT_B_LEFT 1;")
        self.assertEqual(lines[0], "nabos: trying slot B, 0 attempts left after this one")
        booting = next(line for line in lines if line.startswith("nabos: booting slot B:"))
        self.assertIn("root=/dev/mmcblk0p3", booting.split())
        self.assertIn("rauc.slot=B", booting.split())
        self.assertEqual(output.count("Started watchdog@7e100000"), 2)

    def test_missing_watchdog_never_hands_control_to_kernel(self):
        lines, output = self.boot(self.disk("zero-armv6"), watchdog=False)
        self.assertIn("nabos: cannot arm watchdog", lines)
        self.assertNotIn("Unrecognized zImage", output)

    def test_exhausted_slots_restore_attempts_and_reset(self):
        lines, output = self.boot(self.disk("zero-armv6"), "setenv BOOT_A_LEFT 0;")
        self.assertEqual(lines, ["nabos: slot A has no attempts left", "nabos: slot B has no attempts left",
                                 "nabos: no bootable slot, restoring boot attempts"])
        self.assertIn("<reset>", output)

    def test_missing_ab_state_defaults_to_a(self):
        lines, _ = self.boot(self.disk("zero-armv6"), "env delete BOOT_ORDER BOOT_A_LEFT BOOT_B_LEFT;")
        self.assertEqual(lines[0], "nabos: trying slot A, 2 attempts left after this one")
        self.assertIn("nabos: slot B has no attempts left", lines)

    def test_broken_overlay_boots_the_plain_dtb(self):
        lines, _ = self.boot(self.disk("zero-armv6", broken_overlay=True))
        self.assertIn("nabos: overlay failed, using the plain DTB", lines)
        self.assertTrue(any(line.startswith("nabos: booting slot A:") for line in lines))

    def test_arm64_uses_booti(self):
        lines, output = self.boot(self.disk("zero2-arm64"))
        self.assertIn("nabos: overlay tagtagtag-sound applied", lines)
        self.assertIn("nabos: overlay tagtagtag-ears applied", lines)
        self.assertIn("nabos: board revision 0x009000c1", [line.lower() for line in lines])
        self.assertIn("booti_setup", output)
        self.assertNotIn("Unrecognized zImage", output)



def sizes(text):
    units = {"K": 1024, "M": MiB, "G": 1024 * MiB}
    return int(text[:-1]) * units[text[-1]] if text[-1] in units else int(text, 0)


def genimage_partitions():
    cfg = (IMAGE / "genimage.cfg").read_text()
    parts = {}
    for name, body in re.findall(r"partition ([\w-]+) \{(.*?)\}", cfg, re.S):
        parts[name] = dict(re.findall(r"([\w-]+) = \"?([^\"\n]+)\"?", body))
    return parts


class Layout(unittest.TestCase):
    """The same partition and environment constants appear in several files."""

    def test_card_layout(self):
        parts = genimage_partitions()
        self.assertEqual(list(parts), ["uboot-env", "uboot-env-redund", "boot", "rootfs-a", "rootfs-b", "data"])
        self.assertEqual([p.get("in-partition-table") for p in parts.values()][:2], ["false", "false"])
        self.assertEqual(sizes(parts["boot"]["offset"]), 4 * MiB)
        self.assertEqual({k: sizes(parts[k]["size"]) for k in ("boot", "rootfs-a", "rootfs-b", "data")},
                         {"boot": 256 * MiB, "rootfs-a": 6144 * MiB, "rootfs-b": 6144 * MiB, "data": 1024 * MiB})
        self.assertEqual(parts["rootfs-a"]["image"], "rootfs.ext4")
        self.assertNotIn("image", parts["rootfs-b"])  # slot B ships empty
        self.assertEqual({parts[k]["image"] for k in ("boot", "data", "uboot-env", "uboot-env-redund")},
                         {"boot.vfat", "data.ext4", "uboot.env"})
        end = 4 * MiB + (256 + 2 * 6144 + 1024) * MiB
        self.assertLess(end, 15_000_000_000)  # fits the smallest "16 GB" cards

    def test_environment_location_agrees_everywhere(self):
        config = dict(line.split("=", 1) for line in (BOOT / "uboot.config").read_text().splitlines()
                      if line.startswith("CONFIG_"))
        fw_env = [line.split() for line in (ROOTFS / "etc/fw_env.config").read_text().splitlines()
                  if line and not line.startswith("#")]
        parts = genimage_partitions()
        offsets = [int(config["CONFIG_ENV_OFFSET"], 16), int(config["CONFIG_ENV_OFFSET_REDUND"], 16)]
        size = int(config["CONFIG_ENV_SIZE"], 16)
        self.assertEqual(offsets, [0x100000, 0x200000])
        self.assertEqual(size, 0x10000)
        self.assertEqual([sizes(parts[k]["offset"]) for k in ("uboot-env", "uboot-env-redund")], offsets)
        self.assertEqual(fw_env, [["/dev/mmcblk0", hex(o), hex(size)] for o in offsets])
        self.assertLessEqual(offsets[1] + size, sizes(parts["boot"]["offset"]))
        self.assertEqual(config["CONFIG_ENV_REDUNDANT"], "y")
        self.assertIn("# CONFIG_ENV_IS_IN_FAT is not set", (BOOT / "uboot.config").read_text())

    def test_attempts_and_slots_agree(self):
        conf = (ROOTFS / "etc/rauc/system.conf").read_text()
        self.assertIn("boot-attempts=3", conf)
        self.assertIn("boot-attempts-primary=3", conf)
        self.assertRegex(conf, r"device=/dev/mmcblk0p2\ntype=ext4\nbootname=A")
        self.assertRegex(conf, r"device=/dev/mmcblk0p3\ntype=ext4\nbootname=B")
        cmd = (BOOT / "boot.cmd").read_text()
        self.assertIn("setenv BOOT_A_LEFT 3\nsetenv BOOT_B_LEFT 3\nsaveenv\nreset", cmd)
        env = uboot_env()
        self.assertEqual((env["BOOT_ORDER"], env["BOOT_A_LEFT"], env["BOOT_B_LEFT"]), ("A B", "3", "0"))

    def test_stored_environment_matches_pinned_board_defaults(self):
        rpi_env = sorted((SOURCES / "uboot").rglob("board/raspberrypi/rpi/rpi.env"))
        if not rpi_env:
            self.skipTest("pinned U-Boot sources not available")
        pinned = dict(re.findall(r"^(\w+)=(0x[0-9a-fA-F]+)$", rpi_env[0].read_text(), re.M))
        env = uboot_env()
        for key, value in pinned.items():
            self.assertEqual(env.get(key), value, key)
        self.assertIn("boot.scr", env["bootcmd"])


class OverlayFiles(unittest.TestCase):
    def test_scripts_are_executable(self):
        for script in (ROOTFS / "usr/lib/nabos").iterdir():
            self.assertTrue(os.access(script, os.X_OK), script)

    @unittest.skipUnless(shutil.which("git") and (IMAGE.parent / ".git").exists(), "not a git checkout")
    def test_no_overlay_file_is_git_ignored(self):
        files = [str(p.relative_to(IMAGE.parent)) for p in ROOTFS.rglob("*") if p.is_file()]
        ignored = run("git", "-C", IMAGE.parent, "check-ignore", *files, check=False).stdout.split()
        self.assertEqual(ignored, [])


class BootInit(unittest.TestCase):
    SCRIPT = ROOTFS / "usr/lib/nabos/boot-init"

    def test_refuses_to_run_outside_pid1(self):
        result = run("sh", self.SCRIPT, check=False)
        self.assertEqual(result.returncode, 1)
        self.assertIn("must run as PID 1", result.stderr)

    def test_grows_only_the_data_partition_on_a_16gb_card(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            card = tmp / "mmcblk0"
            with open(card, "wb") as f:
                f.truncate(15_931_539_456)  # a real 16 GB card, sparse
            sector = 512
            table = "label: dos\n"
            start = 4 * MiB
            for size, kind in ((256 * MiB, "c"), (6144 * MiB, "83"), (6144 * MiB, "83"), (1024 * MiB, "83")):
                table += f"start={start // sector},size={size // sector},type={kind}\n"
                start += size
            run("sfdisk", "-q", card, input=table)
            before = run("sfdisk", "--json", card).stdout

            def sysfs():
                import json
                parts = json.loads(run("sfdisk", "--json", card).stdout)["partitiontable"]["partitions"]
                block = tmp / "sys/class/block"
                (block / "mmcblk0").mkdir(parents=True, exist_ok=True)
                (block / "mmcblk0p4").mkdir(exist_ok=True)
                (block / "mmcblk0/size").write_text(f"{card.stat().st_size // sector}\n")
                (block / "mmcblk0p4/start").write_text(f"{parts[3]['start']}\n")
                (block / "mmcblk0p4/size").write_text(f"{parts[3]['size']}\n")
                return parts

            kmsg = tmp / "kmsg"
            env = dict(os.environ, NABOS_BOOT_INIT_LIB="1", NABOS_DISK=str(card), NABOS_SYSFS=str(tmp / "sys"),
                       NABOS_KMSG=str(kmsg))
            check = f". {self.SCRIPT}; grow_partition"
            sysfs()
            run("sh", "-c", check, env=env)
            self.assertIn("extending the data partition", kmsg.read_text())
            after = sysfs()
            import json
            old = json.loads(before)["partitiontable"]["partitions"]
            self.assertEqual(old[:3], after[:3])  # boot, A and B untouched
            self.assertEqual(after[3]["start"], old[3]["start"])
            self.assertGreater(after[3]["size"], old[3]["size"])
            self.assertLessEqual(card.stat().st_size // sector - (after[3]["start"] + after[3]["size"]), 2048)
            kmsg.unlink()
            run("sh", "-c", check, env=env)
            self.assertFalse(kmsg.exists())
            self.assertEqual(sysfs(), after)


class FakeCommands:
    """Puts logging stand-ins for system commands first in PATH."""

    def __init__(self, tmp, scripts):
        self.bin = Path(tmp) / "bin"
        self.bin.mkdir()
        self.log = Path(tmp) / "calls"
        self.log.touch()
        for name, body in scripts.items():
            path = self.bin / name
            path.write_text(f"#!/bin/sh\necho \"{name} $*\" >> {self.log}\n{body}\n")
            path.chmod(0o755)

    def env(self, **extra):
        return dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}", **extra)

    def calls(self):
        return self.log.read_text().splitlines()


# pactl list output shaped like pipewire-pulse (LC_ALL=C), trimmed to the fields used.
WM8960_SINK = """Sink #52
	State: SUSPENDED
	Name: alsa_output.platform-soc_sound.stereo-fallback
	Driver: PipeWire
	Properties:
		alsa.card_name = "tagtagtag-sound"
		device.api = "alsa"
		media.class = "Audio/Sink"
"""
NULL_SINK = """Sink #33
	State: SUSPENDED
	Name: auto_null
	Driver: PipeWire
	Properties:
		node.name = "auto_null"
		media.class = "Audio/Sink"
"""
WM8960_MONITOR = """Source #53
	State: SUSPENDED
	Name: alsa_output.platform-soc_sound.stereo-fallback.monitor
	Monitor of Sink: alsa_output.platform-soc_sound.stereo-fallback
	Properties:
		alsa.card_name = "tagtagtag-sound"
		device.class = "monitor"
"""
WM8960_CAPTURE = """Source #54
	State: SUSPENDED
	Name: alsa_input.platform-soc_sound.stereo-fallback
	Monitor of Sink: n/a
	Properties:
		alsa.card_name = "tagtagtag-sound"
		media.class = "Audio/Source"
"""
NULL_MONITOR = """Source #34
	State: SUSPENDED
	Name: auto_null.monitor
	Monitor of Sink: auto_null
	Properties:
		device.class = "monitor"
"""


class Health(unittest.TestCase):
    SCRIPT = ROOTFS / "usr/lib/nabos/health"

    PERSIST = ["/etc/NetworkManager/system-connections", "/var/lib/NetworkManager", "/var/lib/comitup",
               "/var/lib/systemd/timesync", "/var/lib/tagtagtag-sound", "/var/lib/nabos"]

    def mounts(self, **changes):
        """findmnt answers for a correct boot-init run, with optional overrides."""
        table = {"/data": "/dev/mmcblk0p4 ext4", "/etc/machine-id": "/dev/mmcblk0p4[/system/machine-id]"}
        table.update({p: f"/dev/mmcblk0p4[/system{p}]" for p in self.PERSIST})
        table.update(changes)
        return {k: v for k, v in table.items() if v is not None}

    def check(self, *, slot="A", healthy_after=0, own="2", other="3", other_fs="ext4",
              sinks=WM8960_SINK, sources=WM8960_MONITOR + WM8960_CAPTURE, mounts=None):
        with tempfile.TemporaryDirectory() as tmp:
            counter = Path(tmp) / "curl-count"
            counter.write_text("0")
            (Path(tmp) / "sinks").write_text(sinks)
            cases = "".join(f'{k}) echo "{v}";; ' for k, v in (mounts or self.mounts()).items())
            (Path(tmp) / "sources").write_text(sources)
            fake = FakeCommands(tmp, {
                "systemctl": "exit 0",
                "curl": f"n=$(($(cat {counter}) + 1)); echo $n > {counter}; [ $n -gt {healthy_after} ]",
                # runuser -u nabos -- env ... pactl list KIND
                "runuser": '[ "$1 $2 $3" = "-u nabos --" ] || exit 1; shift 3; exec "$@"',
                "pactl": f'[ "$XDG_RUNTIME_DIR $LC_ALL $1" = "/run/user/1000 C list" ] && cat {tmp}/$2',
                # findmnt -n -o COLUMNS --mountpoint PATH
                "findmnt": f'eval "p=\\$$#"; case $p in {cases}*) exit 1;; esac',
                "rauc": "exit 0",
                "blkid": f"echo {other_fs}",
                "fw_printenv": f'case $2 in BOOT_{slot}_LEFT) echo {own};; *) echo {other};; esac',
            })
            cmdline = Path(tmp) / "cmdline"
            cmdline.write_text(f"root=/dev/mmcblk0p2 ro rauc.slot={slot} quiet\n")
            env = fake.env(NABOS_CMDLINE=str(cmdline), NABOS_HEALTH_TIMEOUT="2", NABOS_HEALTH_INTERVAL="1",
                           NABOS_BOOT_INIT=str(ROOTFS / "usr/lib/nabos/boot-init"))
            result = run("sh", self.SCRIPT, env=env, check=False, timeout=30)
            return result, fake.calls()

    def test_healthy_slot_is_marked_good(self):
        result, calls = self.check()
        self.assertIn("rauc status mark-good", calls)
        self.assertNotIn("systemctl reboot", calls)
        self.assertEqual(result.returncode, 0)

    def test_persist_list_matches_boot_init(self):
        persist = re.search(r'^PERSIST="([^"]+)"', (ROOTFS / "usr/lib/nabos/boot-init").read_text(), re.M)
        self.assertEqual(persist.group(1).split(), self.PERSIST)

    def test_volatile_data_is_never_confirmed(self):
        _, calls = self.check(mounts=self.mounts(**{"/data": "tmpfs tmpfs"}))
        self.assertNotIn("rauc status mark-good", calls)

    def test_missing_bind_is_never_confirmed(self):
        for path in ("/etc/machine-id", "/var/lib/nabos", "/etc/NetworkManager/system-connections"):
            with self.subTest(path=path):
                _, calls = self.check(mounts=self.mounts(**{path: None}))
                self.assertNotIn("rauc status mark-good", calls)

    def test_bind_from_elsewhere_is_never_confirmed(self):
        _, calls = self.check(mounts=self.mounts(**{"/var/lib/comitup": "tmpfs"}))
        self.assertNotIn("rauc status mark-good", calls)

    def test_card_name_matches_the_pinned_sound_overlay(self):
        dts = sorted((SOURCES / "sound").rglob("tagtagtag-sound-overlay.dts"))
        if not dts:
            self.skipTest("pinned sound driver sources not available")
        name = re.search(r'simple-audio-card,name = "([^"]+)"', dts[0].read_text()).group(1)
        self.assertIn(f'alsa\\.card_name = "{name}"', self.SCRIPT.read_text())

    def test_null_sink_is_not_audio_hardware(self):
        _, calls = self.check(sinks=NULL_SINK, sources=NULL_MONITOR)
        self.assertNotIn("rauc status mark-good", calls)
        self.assertIn("systemctl reboot", calls)

    def test_monitor_is_not_a_microphone(self):
        _, calls = self.check(sources=NULL_MONITOR + WM8960_MONITOR)
        self.assertNotIn("rauc status mark-good", calls)

    def test_wm8960_next_to_null_sink_is_enough(self):
        _, calls = self.check(sinks=NULL_SINK + WM8960_SINK, sources=WM8960_CAPTURE + NULL_MONITOR)
        self.assertIn("rauc status mark-good", calls)

    def test_unhealthy_slot_with_attempts_reboots(self):
        _, calls = self.check(healthy_after=100)
        self.assertIn("systemctl reboot", calls)
        self.assertNotIn("rauc status mark-good", calls)

    def test_exhausted_slot_falls_back_to_installed_other_slot(self):
        _, calls = self.check(healthy_after=100, own="0", other="3")
        self.assertIn("systemctl reboot", calls)

    def test_no_reboot_loop_when_other_slot_is_empty(self):
        # Initial image: slot B has no filesystem, slot A is out of attempts.
        # Stay up, keep checking, confirm once healthy.
        result, calls = self.check(healthy_after=5, own="0", other="3", other_fs="")
        self.assertNotIn("systemctl reboot", calls)
        self.assertIn("rauc status mark-good", calls)
        self.assertIn("staying up unconfirmed", result.stdout)

    def test_all_attempts_exhausted_stays_up(self):
        result, calls = self.check(slot="B", healthy_after=4, own="0", other="0")
        self.assertNotIn("systemctl reboot", calls)
        self.assertIn("rauc status mark-good", calls)

    def test_not_booted_from_a_slot_does_nothing(self):
        with tempfile.TemporaryDirectory() as tmp:
            fake = FakeCommands(tmp, {"rauc": "exit 0", "systemctl": "exit 0"})
            cmdline = Path(tmp) / "cmdline"
            cmdline.write_text("root=/dev/mmcblk0p2\n")
            run("sh", self.SCRIPT, env=fake.env(NABOS_CMDLINE=str(cmdline),
                                                 NABOS_BOOT_INIT=str(ROOTFS / "usr/lib/nabos/boot-init")))
            self.assertEqual(fake.calls(), [])


class RfidProbe(unittest.TestCase):
    SCRIPT = ROOTFS / "usr/lib/nabos/rfid-probe"

    def probe(self, reg_7f, reg_00):
        with tempfile.TemporaryDirectory() as tmp:
            fake = FakeCommands(tmp, {
                "modprobe": "exit 0",
                "dtoverlay": "exit 0",
                # i2cget -y 1 0x50 REG b; empty value = no answer.
                "i2cget": f'case $4 in 0x7f) v="{reg_7f}";; *) v="{reg_00}";; esac; [ -n "$v" ] && echo $v',
            })
            run("sh", self.SCRIPT, env=fake.env())
            return [c for c in fake.calls() if c.startswith("dtoverlay")]

    def test_st25r391x_2022_nfc_card(self):
        self.assertEqual(self.probe("0x2a", "0x00"), ["dtoverlay -d /boot/overlays st25r391x"])

    def test_cr14_tagtagtag(self):
        self.assertEqual(self.probe("0x13", "0x00"), ["dtoverlay -d /boot/overlays cr14"])

    def test_no_reader(self):
        self.assertEqual(self.probe("", ""), [])


@unittest.skipUnless(shutil.which("node"), "node not available")
class PolkitRule(unittest.TestCase):
    def decide(self, user, action, details=None):
        js = f"""
const polkit = {{ Result: {{ YES: "yes", NOT_HANDLED: "not_handled" }}, rules: [],
                  addRule(f) {{ this.rules.push(f); }} }};
{(ROOTFS / "etc/polkit-1/rules.d/50-nabos.rules").read_text()}
const details = {details or {}!r};
const action = {{ id: {action!r}, lookup: (k) => details[k] }};
console.log(polkit.rules[0](action, {{ user: {user!r} }}));
"""
        return run("node", "-e", js.replace("'", '"')).stdout.strip()

    def test_reboot_voice_and_clock_only(self):
        self.assertEqual(self.decide("nabos", "org.freedesktop.login1.reboot"), "yes")
        self.assertEqual(self.decide("nabos", "org.freedesktop.login1.power-off-multiple-sessions"), "yes")
        unit = {"unit": "linux-voice-assistant.service", "verb": "start"}
        self.assertEqual(self.decide("nabos", "org.freedesktop.systemd1.manage-units", unit), "yes")
        self.assertEqual(self.decide("nabos", "org.freedesktop.systemd1.manage-units",
                                     {"unit": "ssh.service", "verb": "start"}), "not_handled")
        self.assertEqual(self.decide("nabos", "org.freedesktop.systemd1.manage-units",
                                     {"unit": "linux-voice-assistant.service", "verb": "enable"}), "not_handled")
        self.assertEqual(self.decide("nabos", "org.freedesktop.systemd1.manage-unit-files"), "not_handled")
        self.assertEqual(self.decide("nabos", "org.freedesktop.timedate1.set-time"), "yes")
        self.assertEqual(self.decide("nabos", "org.freedesktop.timedate1.set-ntp"), "not_handled")
        for verb in ("start", "stop"):
            self.assertEqual(self.decide("nabos", "org.freedesktop.systemd1.manage-units",
                                         {"unit": "systemd-timesyncd.service", "verb": verb}), "yes")
        self.assertEqual(self.decide("nabos", "org.freedesktop.systemd1.manage-units",
                                     {"unit": "systemd-timesyncd.service", "verb": "restart"}), "not_handled")
        self.assertEqual(self.decide("nobody", "org.freedesktop.login1.reboot"), "not_handled")


class DbusPolicy(unittest.TestCase):
    def test_rauc_installer_only_for_nabos(self):
        import xml.etree.ElementTree as ET
        root = ET.parse(ROOTFS / "etc/dbus-1/system.d/nabos-rauc.conf").getroot()
        policies = {(p.get("context") or p.get("user")): p for p in root.iter("policy")}
        self.assertEqual(policies["default"][0].tag, "deny")
        self.assertEqual(policies["root"][0].attrib, {"send_destination": "de.pengutronix.rauc"})
        members = {(r.get("send_interface"), r.get("send_member")) for r in policies["nabos"]}
        self.assertIn(("de.pengutronix.rauc.Installer", "InstallBundle"), members)
        self.assertFalse({m for m in members if m[0] == "de.pengutronix.rauc.Installer"} - {("de.pengutronix.rauc.Installer", "InstallBundle")})
        self.assertTrue(all(r.tag == "allow" and r.get("send_destination") == "de.pengutronix.rauc" for r in policies["nabos"]))


@unittest.skipUnless(shutil.which("systemd-analyze"), "systemd-analyze not available")
class Units(unittest.TestCase):
    def test_units_parse(self):
        units = sorted((ROOTFS / "usr/lib/systemd/system").glob("*.service"))
        result = run("systemd-analyze", "verify", "--man=no", "--generators=no", *units, check=False)
        noise = ("is not executable", "No such file or directory", "Failed to prepare filename",
                 "Unit is bound to inactive", "not found", "Failed to resolve",
                 "Operation not permitted")
        problems = [line for line in (result.stdout + result.stderr).splitlines()
                    if line.strip() and not any(n in line for n in noise)]
        self.assertEqual(problems, [])


if __name__ == "__main__":
    unittest.main()
