//go:build cgo && (amd64 || 386)

#pragma GCC target("mmx")
#define USE_MMX
#include "../../third_party/aften/libaften/x86/exponent_mmx.c"
