{ lib, stdenvNoCC, stdenv, python313, fetchurl, autoPatchelfHook, makeWrapper,
  alsa-lib, libpulseaudio, mpv-unwrapped, openssl, libffi, zlib,
  coreutils, binutils, cacert, src, version }:
let
  # Reuse the exact CPython 3.13 aarch64 wheels and checksums already locked by
  # the image build. No Debian ELF or runtime pip/venv installation is involved.
  lines = lib.filter (line: line != "" && !(lib.hasPrefix "#" line))
    (lib.splitString "\n" (builtins.readFile ../image/lva-requirements.lock));
  wheels = map (line:
    let parts = builtins.match "[^ ]+ @ (https://[^ ]+) --hash=sha256:([0-9a-f]+)" line;
    in assert parts != null; fetchurl {
      url = builtins.elemAt parts 0;
      sha256 = builtins.elemAt parts 1;
    }) lines;
  # NumPy's locked wheel already includes its matching libgfortran.
  libraries = [ stdenv.cc.cc.lib zlib openssl libffi alsa-lib libpulseaudio mpv-unwrapped ];
in
assert stdenv.hostPlatform.system == "aarch64-linux";
stdenvNoCC.mkDerivation {
  pname = "linux-voice-assistant";
  inherit src version;
  nativeBuildInputs = [ python313 autoPatchelfHook makeWrapper ];
  buildInputs = libraries;
  dontBuild = true;
  dontStrip = true;
  installPhase = ''
    runHook preInstall
    site="$out/share/linux-voice-assistant"
    mkdir -p "$site" "$out/bin"
    python3 - "$site" ${lib.escapeShellArgs (map toString wheels)} <<'PY'
import sys, zipfile
from pathlib import Path
site = Path(sys.argv[1])
for filename in sys.argv[2:]:
    with zipfile.ZipFile(filename) as wheel:
        wheel.extractall(site)
PY
    cp -a linux_voice_assistant sounds wakewords version.txt LICENSE.md "$site/"
    # Runtime dlopen users (python-mpv and SoundCard) also need their libraries.
    makeWrapper ${python313}/bin/python3 "$out/bin/linux-voice-assistant" \
      --add-flags '-m linux_voice_assistant' \
      --add-flags '--preferences-file /var/lib/nabos/lva/preferences.json' \
      --add-flags '--download-dir /var/lib/nabos/lva/wakewords' \
      --set PYTHONPATH "$site" \
      --set PYTHONDONTWRITEBYTECODE 1 \
      --set HOME /var/lib/nabos/lva \
      --set XDG_DATA_HOME /var/lib/nabos/lva/data \
      --set XDG_CONFIG_HOME /var/lib/nabos/lva/config \
      --set XDG_CACHE_HOME /var/lib/nabos/lva/cache \
      --set SSL_CERT_FILE ${cacert}/etc/ssl/certs/ca-bundle.crt \
      --prefix PATH : ${lib.makeBinPath [ coreutils binutils ]} \
      --prefix LD_LIBRARY_PATH : ${lib.makeLibraryPath libraries}
    runHook postInstall
  '';
  doInstallCheck = stdenv.buildPlatform.canExecute stdenv.hostPlatform;
  installCheckPhase = ''
    # SoundCard connects to PulseAudio on import; audio-server integration is
    # exercised by the system test, not inside this isolated package builder.
    PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$out/share/linux-voice-assistant" \
      PATH=${lib.makeBinPath [ coreutils binutils ]}:$PATH \
      LD_LIBRARY_PATH=${lib.makeLibraryPath libraries} python3 - <<'PY'
import aioesphomeapi, numpy, mpv, pymicro_wakeword, pyopen_wakeword, webrtc_noise_gain
import ctypes
ctypes.CDLL("libpulse.so")
from pathlib import Path
import linux_voice_assistant
root = Path(linux_voice_assistant.__file__).parent.parent
assert (root / "sounds").is_dir()
assert (root / "wakewords").is_dir()
assert (root / "version.txt").is_file()
for source in (root / "linux_voice_assistant").rglob("*.py"):
    compile(source.read_bytes(), str(source), "exec")
PY
  '';
  meta = {
    description = "Linux Voice Assistant with immutable dependencies and persistent model state";
    platforms = [ "aarch64-linux" ];
    mainProgram = "linux-voice-assistant";
    license = lib.licenses.asl20;
  };
}
