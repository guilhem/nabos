{ lib, runCommand, stdenv, python313, fetchurl, makeWrapper, unzip, zlib, lva }:
let
  wheel = file: sha256: fetchurl {
    url = "https://files.pythonhosted.org/packages/${file}";
    inherit sha256;
  };
  wheels = [
    (wheel "5e/90/de832b09736db8c26c9b5dd25cb408ed065b1d9535e05c78947aec056e36/piper_tts-1.8.0-cp39-abi3-manylinux_2_17_aarch64.manylinux2014_aarch64.manylinux_2_28_aarch64.whl"
      "3f60c1917de6d8e8033f395878ad3f88f6dfee88a8b05f98971a275f76a38484")
    (wheel "e1/e1/633d4994db0620d31f34d9962119bc4ebd0b2ef4c9a030f91c7843fb9672/onnxruntime-1.31.1-cp313-cp313-manylinux_2_28_aarch64.whl"
      "dcaedc50333876f3829ab966b242147f0e548229e29c36fe44622ecf748c25ee")
    (wheel "9a/70/875f4a23bfc4731703a5835487d0d2fb999031bd415e7d17c0ae615c18b7/pathvalidate-3.3.1-py3-none-any.whl"
      "5263baab691f8e1af96092fa5137ee17df5bdfbd6cff1fcac4d6ef4bc2e1735f")
    (wheel "e8/2d/d2a548598be01649e2d46231d151a6c56d10b964d94043a335ae56ea2d92/flatbuffers-25.12.19-py2.py3-none-any.whl"
      "7634f50c427838bb021c2d66a3d1168e9d199b0607e6329399f04846d42e20b4")
  ];
  sharedSite = "${lva}/share/linux-voice-assistant";
in
assert stdenv.hostPlatform.system == "aarch64-linux";
runCommand "nabos-piper-1.8.0" { nativeBuildInputs = [ unzip makeWrapper ]; } ''
  site="$out/share/nabos-piper"
  mkdir -p "$site" "$out/bin"
  for archive in ${lib.escapeShellArgs (map toString wheels)}; do
    unzip -q "$archive" -d "$site"
  done
  # The Python binding embeds the CPU runtime. The French voice uses eSpeak.
  rm -r "$site"/onnxruntime/{capi/libonnxruntime.so.1.31.1,quantization,transformers} \
    "$site"/piper/{train,hebrew/nakdimon.onnx,tashkeel/model.onnx}
  # Both programs use the ARM64 CPython 3.13 wheels locked by the LVA package.
  for dep in ${sharedSite}/{numpy,numpy.libs,numpy-*.dist-info,google,protobuf-*.dist-info,packaging,packaging-*.dist-info}; do
    test -d "$dep"
    ln -s "$dep" "$site/"
  done
  makeWrapper ${python313}/bin/python3 "$out/bin/nabos-piper" \
    --add-flags ${./piper_synthesize.py} --set PYTHONPATH "$site" \
    --set PYTHONDONTWRITEBYTECODE 1 --set PYTHONNOUSERSITE 1 \
    --set ORT_DISABLE_TELEMETRY 1 --set OPENBLAS_NUM_THREADS 1 --set OMP_NUM_THREADS 1 \
    --set LD_LIBRARY_PATH ${lib.makeLibraryPath [ stdenv.cc.cc.lib zlib ]}
''
