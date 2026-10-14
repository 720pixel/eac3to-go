//go:build cgo && (amd64 || 386)

#pragma GCC target("sse3")
#define USE_MMX
#define USE_SSE
#define USE_SSE2
#define USE_SSE3
#include "../../third_party/aften/libaften/x86/mdct_sse3.c"
