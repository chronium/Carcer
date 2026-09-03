#pragma once
#include <stdint.h>
struct task_context{uint64_t r15,r14,r13,r12,r11,r10,r9,r8,rdi,rsi,rbp,rdx,rcx,rbx,rax,rip,cs,rflags,rsp,ss;};int task_init(void);int task_new(void(*)(void));int task_user(const uint8_t*,uint32_t,uint32_t);int task_file(const uint8_t*,uint32_t);int task_kill(uint32_t);int task_wait(uint32_t,uint64_t*);struct task_context*task_sched(struct task_context*,int);struct task_context*task_syscall(struct task_context*);
