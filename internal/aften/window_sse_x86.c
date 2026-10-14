//go:build cgo && (amd64 || 386)

#pragma GCC target("sse")
#define USE_MMX
#define USE_SSE
#include "../../third_party/aften/libaften/x86/window_sse.c"
