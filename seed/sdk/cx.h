#ifndef CX_H
#define CX_H
#include <stdint.h>
#include <stddef.h>
#define CX_ERROR UINT64_MAX
/* These calls preserve GPRs except RAX. Buffers use byte spans, not C strings. */
static inline uint64_t cx_call(uint64_t n, uint64_t a, uint64_t b,
                               uint64_t c, uint64_t d, uint64_t e, uint64_t f) {
    register uint64_t r8 __asm__("r8") = e, r9 __asm__("r9") = f;
    __asm__ volatile("int $0x80" : "+a"(n)
        : "D"(a), "S"(b), "d"(c), "c"(d), "r"(r8), "r"(r9)
        : "memory", "cc");
    return n;
}
__attribute__((noreturn)) static inline void cx_exit(uint64_t status) {
    cx_call(0,status,0,0,0,0,0);
    for (;;) __asm__ volatile("ud2");
}
static inline uint64_t cx_size(const char *p,size_t n) {
    return cx_call(1,(uintptr_t)p,n,0,0,0,0);
}
static inline uint64_t cx_read(const char *p,size_t n,uint64_t off,void *b,size_t z) {
    return cx_call(2,(uintptr_t)p,n,off,(uintptr_t)b,z,0);
}
static inline uint64_t cx_attributes(const char *p,size_t n) {
    return cx_call(3,(uintptr_t)p,n,0,0,0,0);
}
static inline uint64_t cx_write(const char *p,size_t n,uint64_t off,const void *b,size_t z) {
    return cx_call(4,(uintptr_t)p,n,off,(uintptr_t)b,z,0);
}
static inline uint64_t cx_spawn(const char *p,size_t n) {
    return cx_call(5,(uintptr_t)p,n,0,0,0,0);
}
static inline uint64_t cx_reap(uint64_t task,uint64_t *status) {
    return cx_call(6,task,(uintptr_t)status,0,0,0,0);
}
static inline uint64_t cx_brk(uint64_t end) { return cx_call(7,end,0,0,0,0,0); }
static inline uint64_t cx_ticks(void) { return cx_call(8,0,0,0,0,0,0); }
struct cx_display { uint32_t size,width,height,pitch,format,reserved[3]; };
static inline uint64_t cx_display_info(struct cx_display *v) {
    return cx_call(9,(uintptr_t)v,sizeof(*v),0,0,0,0);
}
static inline uint64_t cx_present(const void *p,size_t stride,
                                 uint32_t x,uint32_t y,uint32_t w,uint32_t h) {
    return cx_call(10,(uintptr_t)p,stride,x,y,w,h);
}
static inline uint64_t cx_sleep(uint64_t ticks) { return cx_call(11,ticks,0,0,0,0,0); }
static inline uint64_t cx_wait(uint64_t task,uint64_t *status) {
    return cx_call(12,task,(uintptr_t)status,0,0,0,0);
}
void *memcpy(void *,const void *,size_t);
void *memmove(void *,const void *,size_t);
void *memset(void *,int,size_t);
int memcmp(const void *,const void *,size_t);
size_t strlen(const char *);
/* Monotonic, zero-initialized allocation. No free; do not mix with direct brk. */
void *cx_alloc(size_t);
#endif
