#pragma once
#include <stdint.h>
struct task_context{uint64_t r15,r14,r13,r12,r11,r10,r9,r8,rdi,rsi,rbp,rdx,rcx,rbx,rax,rip,cs,rflags,rsp,ss;};int tasks_init(void);int task_create(void(*)(void));int task_create_user(const uint8_t*,uint32_t,uint32_t);int task_create_user_file(const uint8_t*,uint32_t);int task_destroy(uint32_t);int task_reap(uint32_t,uint64_t*);struct task_context*tasks_schedule(struct task_context*,int);struct task_context*tasks_system_call(struct task_context*);
