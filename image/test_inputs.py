"""Exercise input integrity and archive path boundaries without network or root."""

import hashlib
import io
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from fetch import download, unpack


class AssetsTest(unittest.TestCase):
    def test_builtin_resources_are_self_contained(self):
        assets = Path(__file__).resolve().parents[1] / "assets"
        sounds = assets / "sounds"
        patterns = ["system/abort.wav", "clock/signature.mp3",
                    "weather/today.mp3", "weather/sky/*.mp3",
                    "sleep/*.mp3", "wakeup/*.mp3"]
        patterns += [f"clock/{hour}/*.mp3" for hour in range(24)]
        for pattern in patterns:
            with self.subTest(resource=pattern):
                self.assertTrue(any(p.is_file() for base in (sounds / "fr_FR", sounds)
                                    for p in base.glob(pattern)))
        for pattern in ("system/rfid.chor", "system/streaming/*.chor", "taichi/*.chor"):
            with self.subTest(choreography=pattern):
                self.assertTrue(any(p.is_file() for p in (assets / "choreographies").glob(pattern)))
        for path in assets.rglob("*"):
            if path.is_symlink():
                self.assertTrue(path.resolve(strict=True).is_relative_to(assets))


class InputsTest(unittest.TestCase):
    def test_verified_download_is_atomic(self):
        with tempfile.TemporaryDirectory() as tmp:
            dest = Path(tmp) / "source.tar.gz"
            dest.write_bytes(b"previous")
            expected = hashlib.sha256(b"reviewed").hexdigest()
            with patch("urllib.request.urlopen", side_effect=lambda *a, **k: io.BytesIO(b"tampered")), patch("time.sleep"):
                with self.assertRaises(ValueError):
                    download("https://example.org/source", expected, dest)
            self.assertEqual(dest.read_bytes(), b"previous")
            self.assertFalse(dest.with_suffix(".gz.part").exists())
            with patch("urllib.request.urlopen", return_value=io.BytesIO(b"reviewed")):
                download("https://example.org/source", expected, dest)
            self.assertEqual(dest.read_bytes(), b"reviewed")
            with patch("urllib.request.urlopen", side_effect=AssertionError("unexpected network")):
                download("https://example.org/source", expected, dest)

    def test_archive_cannot_escape(self):
        for name in ("../outside", "/absolute/file", "source/../../outside"):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                path = Path(tmp)
                archive = path / "source.tar.gz"
                with tarfile.open(archive, "w:gz") as tar:
                    entry = tarfile.TarInfo(name)
                    entry.size = 1
                    tar.addfile(entry, io.BytesIO(b"x"))
                with self.assertRaises((ValueError, tarfile.FilterError)):
                    unpack(archive, path / "out")
                self.assertFalse((path / "outside").exists())

    def test_archive_rejects_escaping_symlink(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            archive = path / "source.tar.gz"
            with tarfile.open(archive, "w:gz") as tar:
                entry = tarfile.TarInfo("source/link")
                entry.type = tarfile.SYMTYPE
                entry.linkname = "../../outside"
                tar.addfile(entry)
            with self.assertRaises(tarfile.FilterError):
                unpack(archive, path / "out")

    def test_archive_extracts_one_source_tree(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            archive = path / "source.tar.gz"
            with tarfile.open(archive, "w:gz") as tar:
                entry = tarfile.TarInfo("source/file")
                entry.size = 1
                tar.addfile(entry, io.BytesIO(b"x"))
            self.assertEqual((unpack(archive, path / "out") / "file").read_bytes(), b"x")


if __name__ == "__main__":
    unittest.main()
