//go:build cgo && (amd64 || 386)

#pragma GCC target("sse2")
#define USE_MMX
#define USE_SSE
#define USE_SSE2
#include "../../third_party/aften/libaften/x86/exponent_sse2.c"
