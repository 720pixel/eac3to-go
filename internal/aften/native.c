//go:build cgo

#include "native.h"
#include "../fpenv/fpenv.h"
#include "aften.h"
#include "a52enc.h"
#include "crc.h"

struct NativeAftenEncoder {
    AftenContext context;
    struct {
        uint32_t bit_count, sample_count;
        int last_quality;
    } frame_state[32];
    int frame_contexts, next_context;
};

static NativeAftenEncoder *aften_create_impl(int sample_rate, int channels, int acmod,
                                      int lfe, int bitrate, int frame_contexts) {
    if (frame_contexts < 1 || frame_contexts > 32) return NULL;
    NativeAftenEncoder *encoder = calloc(1, sizeof(*encoder));
    if (!encoder) return NULL;
    AftenContext *context = &encoder->context;
    aften_set_defaults(context);
    context->system.n_threads = 1;
    context->sample_format = A52_SAMPLE_FMT_DBL;
    context->samplerate = sample_rate;
    context->channels = channels;
    context->acmod = acmod;
    context->lfe = lfe;
    context->params.bitrate = bitrate;
    if (aften_encode_init(context) != 0) {
        aften_encode_close(context);
        free(encoder);
        return NULL;
    }
    encoder->frame_contexts = frame_contexts;
    A52Context *private_context = context->private_context;
    for (int i = 0; i < frame_contexts; ++i)
        encoder->frame_state[i].last_quality = private_context->tctx->last_quality;
    return encoder;
}

static int aften_encode_impl(NativeAftenEncoder *encoder, const double *samples,
                        int count, uint8_t *output, size_t capacity) {
    if (!encoder || !output || capacity < A52_MAX_CODED_FRAME_SIZE) return -1;
    A52Context *context = encoder->context.private_context;
    A52ThreadContext *frame = context->tctx;
    int index = encoder->next_context;
    frame->bit_cnt = encoder->frame_state[index].bit_count;
    frame->sample_cnt = encoder->frame_state[index].sample_count;
    frame->last_quality = encoder->frame_state[index].last_quality;
    int size = aften_encode_frame(&encoder->context, output, samples, count);
    if (size > 0) {
        encoder->frame_state[index].bit_count = frame->bit_cnt;
        encoder->frame_state[index].sample_count = frame->sample_cnt;
        encoder->frame_state[index].last_quality = frame->last_quality;
        encoder->next_context = (index + 1) % encoder->frame_contexts;
    }
    return size;
}

void native_aften_destroy(NativeAftenEncoder *encoder) {
    if (!encoder) return;
    aften_encode_close(&encoder->context);
    free(encoder);
}

typedef struct {
    const uint8_t *data;
    size_t bit, end;
    int error;
} HeaderBits;

static unsigned read_bits(HeaderBits *b, unsigned count) {
    unsigned value = 0;
    if (count > 32 || b->bit > b->end || count > b->end - b->bit) {
        b->error = 1;
        return 0;
    }
    while (count--) {
        value = (value << 1) | ((b->data[b->bit / 8] >> (7 - b->bit % 8)) & 1);
        ++b->bit;
    }
    return value;
}

static int source_context(AftenContext *c, const uint8_t *frame, size_t length) {
    static const int rates[] = {48000, 44100, 32000};
    static const int bitrates[] = {32,40,48,56,64,80,96,112,128,160,192,224,256,320,384,448,512,576,640};
    static const int channels[] = {2,1,2,3,3,4,4,5};
    if (length < 8 || frame[0] != 0x0b || frame[1] != 0x77 || (frame[5] >> 3) > 16)
        return -1;
    /* Do not emit a full-rate frame into an unvalidated reduced-rate stream. */
    if ((frame[5] >> 3) > 8) return -6;
    if ((frame[4] >> 6) == 3 || ((frame[4] & 63) >> 1) >= 19) return -1;

    c->params.bitrate = bitrates[(frame[4] & 63) >> 1];
    c->samplerate = rates[frame[4] >> 6];
    int size = c->params.bitrate * 192000 / c->samplerate;
    if (c->samplerate == 44100) size = (size & ~1) + (frame[4] & 1) * 2;
    if (length != (size_t)size) return -1;
    int split = (size / 4 + size / 16) * 2;
    crc_init();
    if (calc_crc16(frame + 2, split - 2) != 0 || calc_crc16(frame + split, size - split) != 0)
        return -2;

    HeaderBits bits = {frame, 40, (length - 2) * 8, 0};
    unsigned bsid = read_bits(&bits, 5);
    read_bits(&bits, 3); /* bsmod is not copied by the historical generator. */
    c->acmod = read_bits(&bits, 3);
    if ((c->acmod & 1) && c->acmod != 1) c->meta.cmixlev = read_bits(&bits, 2);
    if (c->acmod & 4) c->meta.surmixlev = read_bits(&bits, 2);
    if (c->acmod == 2) c->meta.dsurmod = read_bits(&bits, 2);
    c->lfe = read_bits(&bits, 1);
    c->channels = channels[c->acmod] + c->lfe;
    c->meta.dialnorm = read_bits(&bits, 5);
    if (bsid == 6) {
        if (read_bits(&bits, 1)) read_bits(&bits, 8);
        if (read_bits(&bits, 1)) read_bits(&bits, 8);
        if (read_bits(&bits, 1)) read_bits(&bits, 7);
        if (c->acmod == 0) {
            read_bits(&bits, 5);
            if (read_bits(&bits, 1)) read_bits(&bits, 8);
            if (read_bits(&bits, 1)) read_bits(&bits, 8);
            if (read_bits(&bits, 1)) read_bits(&bits, 7);
        }
        read_bits(&bits, 2); /* copyright and original bitstream */
        c->meta.xbsi1e = read_bits(&bits, 1);
        if (c->meta.xbsi1e) {
            c->meta.dmixmod = read_bits(&bits, 2);
            c->meta.ltrtcmixlev = read_bits(&bits, 3);
            c->meta.ltrtsmixlev = read_bits(&bits, 3);
            c->meta.lorocmixlev = read_bits(&bits, 3);
            c->meta.lorosmixlev = read_bits(&bits, 3);
        }
        c->meta.xbsi2e = read_bits(&bits, 1);
        if (c->meta.xbsi2e) {
            c->meta.dsurexmod = read_bits(&bits, 2);
            c->meta.dheadphonmod = read_bits(&bits, 2);
            c->meta.adconvtyp = read_bits(&bits, 1);
        }
    }
    return bits.error ? -1 : 0;
}

static int aften_silence_impl(const uint8_t *frame, size_t length, uint8_t *output, size_t capacity) {
    AftenContext context;
    if (!frame || !output || capacity < A52_MAX_CODED_FRAME_SIZE) return -1;
    memset(&context, 0, sizeof(context));
    aften_set_defaults(&context);
    context.system.n_threads = 1;
    context.sample_format = A52_SAMPLE_FMT_S16;
    int result = source_context(&context, frame, length);
    if (result) return result;
    int expected = context.params.bitrate * 192000 / context.samplerate;
    if (aften_encode_init(&context) != 0) {
        if (context.private_context) aften_encode_close(&context);
        return -3;
    }
    int16_t *pcm = calloc((size_t)context.channels * A52_SAMPLES_PER_FRAME, sizeof(*pcm));
    if (!pcm) {
        aften_encode_close(&context);
        return -4;
    }
    result = -5;
    for (int attempt = 0; attempt < 3; ++attempt) {
        int size = aften_encode_frame(&context, output, pcm, A52_SAMPLES_PER_FRAME);
        if (size == expected) { result = size; break; }
        if (size < 0) break;
    }
    free(pcm);
    aften_encode_close(&context);
    return result;
}

NativeAftenEncoder *native_aften_create(int sample_rate, int channels, int acmod,
                                      int lfe, int bitrate, int frame_contexts) {
    unsigned short environment = native_fpenv_enter();
    NativeAftenEncoder *encoder = aften_create_impl(sample_rate, channels, acmod,
                                                   lfe, bitrate, frame_contexts);
    native_fpenv_leave(environment);
    return encoder;
}

int native_aften_encode(NativeAftenEncoder *encoder, const double *samples,
                       int count, uint8_t *output, size_t capacity) {
    unsigned short environment = native_fpenv_enter();
    int result = aften_encode_impl(encoder, samples, count, output, capacity);
    native_fpenv_leave(environment);
    return result;
}

int native_aften_silence(const uint8_t *frame, size_t length, uint8_t *output, size_t capacity) {
    unsigned short environment = native_fpenv_enter();
    int result = aften_silence_impl(frame, length, output, capacity);
    native_fpenv_leave(environment);
    return result;
}
