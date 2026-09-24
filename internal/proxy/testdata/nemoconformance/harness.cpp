// SPDX-License-Identifier: Apache-2.0
//
// Differential harness: runs NeMo-Speech.cpp's REAL wav intake over files named
// on the command line and reports, per file, what its parser did.
//
// It links NVIDIA/NeMo-Speech.cpp's own src/common/audio_file.cpp. That
// translation unit includes only its own header and the standard library — no
// ggml, no CUDA, no model — so the intake can be exercised without building or
// running NeMo. This tests the PARSER, which is the intake. It does not run
// recognition and loads no model.
//
// Build and usage: see README.md in this directory.
//
// Output is one line per input, tab-separated, for a test harness to parse:
//
//	<path>\tACCEPT\trate=<hz>\tchannels=<n>\tframes=<n>
//	<path>\tREJECT\t<message>
//
// The reject message is NeMo's own text, with the leading "<source>: " that
// file_error() prepends stripped, so the caller sees the reason alone.

#include "audio_file.h"

#include <cstdio>
#include <fstream>
#include <iostream>
#include <string>
#include <vector>

namespace {

// read_file slurps a file whole. Returns false when it cannot be opened.
bool
read_file(const std::string& path, std::vector<unsigned char>* out) {
    std::ifstream input(path, std::ios::binary | std::ios::ate);
    if (!input)
        return false;
    const auto end = input.tellg();
    if (end < 0)
        return false;
    out->resize(static_cast<size_t>(end));
    input.seekg(0);
    if (out->empty())
        return true;
    return static_cast<bool>(
        input.read(reinterpret_cast<char*>(out->data()), static_cast<std::streamsize>(out->size())));
}

// strip_source removes the "<source_name>: " prefix file_error() adds, leaving
// only the reason. We always pass the same source name, so the prefix is known.
std::string
strip_source(const std::string& message, const std::string& source) {
    const std::string prefix = source + ": ";
    if (message.rfind(prefix, 0) == 0)
        return message.substr(prefix.size());
    return message;
}

}  // namespace

int
main(int argc, char** argv) {
    if (argc < 2) {
        std::fprintf(stderr, "usage: %s <wav-file>...\n", argv[0]);
        return 2;
    }
    // A fixed source name keeps the reject text free of the (private) path.
    const std::string source = "audio upload";
    for (int i = 1; i < argc; ++i) {
        const std::string path = argv[i];
        std::vector<unsigned char> bytes;
        if (!read_file(path, &bytes)) {
            std::cout << path << "\tERROR\tcannot read file\n";
            continue;
        }
        try {
            const auto audio = nemo_speech::audio::load_wav_memory(
                bytes.empty() ? nullptr : bytes.data(), bytes.size(), source);
            std::cout << path << "\tACCEPT"
                      << "\trate=" << audio.sample_rate << "\tchannels=" << audio.source_channels
                      << "\tframes=" << audio.samples.size() << "\n";
        } catch (const std::exception& error) {
            std::cout << path << "\tREJECT\t" << strip_source(error.what(), source) << "\n";
        }
    }
    return 0;
}
