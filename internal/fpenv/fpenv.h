#ifndef EAC3TO_FPENV_H
#define EAC3TO_FPENV_H

/* Windows worker threads may begin with 53-bit x87 precision while the initial
 * thread has 64-bit precision. Keep native filter initialization and processing
 * independent of the calling thread, then restore its control word. */
static inline unsigned short native_fpenv_enter(void)
{
#if defined(_WIN32) && (defined(__i386__) || defined(__x86_64__)) && defined(__GNUC__)
    unsigned short previous, extended;
    __asm__ volatile ("fnstcw %0" : "=m"(previous));
    extended = (previous & ~0x0300u) | 0x0300u;
    __asm__ volatile ("fldcw %0" : : "m"(extended));
    return previous;
#else
    return 0;
#endif
}

static inline void native_fpenv_leave(unsigned short previous)
{
#if defined(_WIN32) && (defined(__i386__) || defined(__x86_64__)) && defined(__GNUC__)
    __asm__ volatile ("fldcw %0" : : "m"(previous));
#else
    (void)previous;
#endif
}

#ifdef __cplusplus
class NativeFloatEnvironment {
    unsigned short previous;
public:
    NativeFloatEnvironment() : previous(native_fpenv_enter()) {}
    ~NativeFloatEnvironment() { native_fpenv_leave(previous); }
    NativeFloatEnvironment(const NativeFloatEnvironment &) = delete;
    NativeFloatEnvironment &operator=(const NativeFloatEnvironment &) = delete;
};
#endif

#endif
