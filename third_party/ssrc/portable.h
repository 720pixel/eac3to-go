#ifndef EAC3TO_SSRC_PORTABLE_H
#define EAC3TO_SSRC_PORTABLE_H

#if defined(__clang__)
#pragma clang fp contract(off)
#elif defined(__GNUC__)
#pragma GCC optimize ("fp-contract=off")
#endif

#include <algorithm>
#include <cstdlib>
#include <cstring>
#include <cstdint>

#ifndef __fastcall
#define __fastcall
#endif
#ifndef _inline
#define _inline inline
#endif
using std::max;

// This helper is used only for the latency query, not sample processing.
inline int MulDiv(int value, int multiplier, int divisor) {
    if (divisor == 0) return -1;
    int64_t product = static_cast<int64_t>(value) * multiplier;
    return static_cast<int>((product + divisor / 2) / divisor);
}

#endif
