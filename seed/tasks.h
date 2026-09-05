#pragma once
#include <stdint.h>
struct tc{uint8_t fx[512];uint64_t r15,r14,r13,r12,r11,r10,r9,r8,rdi,rsi,rbp,rdx,rcx,rbx,rax,rip,cs,rflags,rsp,ss;}__attribute__((aligned(16)));int tinit(void);int tnew(void(*)(void));int tuser(const uint8_t*,uint32_t,uint32_t);int tfile(const uint8_t*,uint32_t);int tkill(uint32_t);int twait(uint32_t,uint64_t*);struct tc*tsched(struct tc*,int);struct tc*tsys(struct tc*);

_Static_assert(sizeof(struct tc)==672,"context size");
_Static_assert(__builtin_offsetof(struct tc,r15)==512,"context layout");
