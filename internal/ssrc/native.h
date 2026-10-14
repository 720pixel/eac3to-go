#ifndef EAC3TO_SSRC_NATIVE_H
#define EAC3TO_SSRC_NATIVE_H

#ifdef __cplusplus
extern "C" {
#endif

typedef void *ssrc_handle;
ssrc_handle native_ssrc_new(int input_rate, int output_rate, int channels,
                            double attenuation, double transition_band, int fft_length);
int native_ssrc_write(ssrc_handle handle, const double *input, unsigned int bytes);
unsigned int native_ssrc_size(ssrc_handle handle);
int native_ssrc_read(ssrc_handle handle, double *output, unsigned int bytes);
int native_ssrc_finish(ssrc_handle handle);
unsigned int native_ssrc_drain_input_frames(ssrc_handle handle);
void native_ssrc_close(ssrc_handle handle);

#ifdef __cplusplus
}
#endif
#endif
