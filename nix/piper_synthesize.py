"""Render UTF-8 text from stdin with the installed French voice."""

import argparse
import json
import sys
import wave
from importlib.metadata import version

import onnxruntime as ort
from piper import PiperConfig, PiperVoice


def main():
    ort.disable_telemetry_events()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", action="version",
                        version=f"Piper {version('piper-tts')}, ONNX Runtime {ort.__version__}")
    parser.add_argument("model", metavar="MODEL.onnx")
    parser.add_argument("output", metavar="OUTPUT.wav")
    args = parser.parse_args()
    text = sys.stdin.buffer.read().decode("utf-8").strip()
    if not text:
        parser.error("expected text on stdin")

    with open(args.model + ".json", encoding="utf-8") as config_file:
        config = PiperConfig.from_dict(json.load(config_file))
    # These settings keep a French utterance within the Pi Zero 2's memory budget.
    options = ort.SessionOptions()
    options.intra_op_num_threads = 2
    options.inter_op_num_threads = 1
    options.enable_cpu_mem_arena = False
    options.enable_mem_pattern = False
    session = ort.InferenceSession(args.model, sess_options=options, providers=["CPUExecutionProvider"])
    voice = PiperVoice(session=session, config=config)
    with wave.open(args.output, "wb") as audio:
        voice.synthesize_wav(text, audio)


if __name__ == "__main__":
    main()
