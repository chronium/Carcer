#ifndef CODEXOS_SEED_TASKS_H
#define CODEXOS_SEED_TASKS_H
#include <stdint.h>
struct task_context{uint64_t r15,r14,r13,r12,r11,r10,r9,r8;uint64_t rdi,rsi,rbp,rdx,rcx,rbx,rax;uint64_t rip,cs,rflags,rsp,ss;};int tasks_init(void);int task_create(void(*entry)(void));int task_create_user(const uint8_t*image,uint32_t size,uint32_t entry_offset);int task_create_user_file(const uint8_t*path,uint32_t path_length);int task_destroy(uint32_t identifier);struct task_context*tasks_schedule(struct task_context*frame);struct task_context*tasks_system_call(struct task_context*frame);
#endif
