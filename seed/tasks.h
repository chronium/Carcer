#pragma once
#include <stdint.h>
struct tc{uint64_t r15,r14,r13,r12,r11,r10,r9,r8,rdi,rsi,rbp,rdx,rcx,rbx,rax,rip,cs,rflags,rsp,ss;};int tinit(void);int tnew(void(*)(void));int tuser(const uint8_t*,uint32_t,uint32_t);int tfile(const uint8_t*,uint32_t);int tkill(uint32_t);int twait(uint32_t,uint64_t*);struct tc*tsched(struct tc*,int);struct tc*tsys(struct tc*);
