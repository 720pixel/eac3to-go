#ifndef EAC3TO_AFTEN_NATIVE_H
#define EAC3TO_AFTEN_NATIVE_H
#include <stdint.h>
#include <stddef.h>
int native_aften_silence(const uint8_t *frame, size_t length, uint8_t *output, size_t capacity);
typedef struct NativeAftenEncoder NativeAftenEncoder;
NativeAftenEncoder *native_aften_create(int sample_rate, int channels, int acmod,
                                      int lfe, int bitrate, int frame_contexts);
int native_aften_encode(NativeAftenEncoder *encoder, const double *samples,
                        int count, uint8_t *output, size_t capacity);
void native_aften_destroy(NativeAftenEncoder *encoder);
#endif
