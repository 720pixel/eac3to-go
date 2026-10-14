//go:build cgo

#include "native.h"
#include "../fpenv/fpenv.h"
#include "../../third_party/ssrc/ssrc.h"

extern "C" {

ssrc_handle native_ssrc_new(int input_rate, int output_rate, int channels,
                            double attenuation, double transition_band, int fft_length) {
    NativeFloatEnvironment floating_point;
    try {
        Resampler_base::CONFIG config(input_rate, output_rate, channels,
                                      attenuation, transition_band, fft_length);
        return Resampler_base::Create(config);
    } catch (...) {
        return nullptr;
    }
}

int native_ssrc_write(ssrc_handle handle, const double *input, unsigned int bytes) {
    NativeFloatEnvironment floating_point;
    if (!handle || (!input && bytes)) return 0;
    try {
        static_cast<Resampler_base *>(handle)->Write(const_cast<double *>(input), bytes);
        return 1;
    } catch (...) {
        return 0;
    }
}

unsigned int native_ssrc_size(ssrc_handle handle) {
    if (!handle) return 0;
    return static_cast<Resampler_base *>(handle)->GetDataInOutbuf();
}

int native_ssrc_read(ssrc_handle handle, double *output, unsigned int bytes) {
    if (!handle || (!output && bytes)) return 0;
    auto *resampler = static_cast<Resampler_base *>(handle);
    unsigned int size = 0;
    void *buffer = resampler->GetBuffer(&size);
    if (bytes > size) return 0;
    if (bytes) {
        std::memcpy(output, buffer, bytes);
        resampler->Read(bytes);
    }
    return 1;
}

int native_ssrc_finish(ssrc_handle handle) {
    NativeFloatEnvironment floating_point;
    if (!handle) return 0;
    try {
        static_cast<Resampler_base *>(handle)->Finish();
        return 1;
    } catch (...) {
        return 0;
    }
}

void native_ssrc_close(ssrc_handle handle) {
    delete static_cast<Resampler_base *>(handle);
}

unsigned int native_ssrc_drain_input_frames(ssrc_handle handle) {
    if (!handle) return 0;
    return static_cast<Resampler_base *>(handle)->GetDrainInputFrames();
}

}
