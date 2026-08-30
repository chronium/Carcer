#include "tasks.h"
#include "interrupts.h"
#define TASK_MAX_COUNT 4u
#define TASK_STACK_SIZE (16u * 1024u)
#define TASK_CONTEXT_WORDS (sizeof(struct task_context)/sizeof(uint64_t))
#define KERNEL_CODE_SELECTOR 0x08u
#define KERNEL_DATA_SELECTOR 0x10u
struct task_slot {
struct task_context context;
uint8_t runnable;
};
static struct task_slot task_slots[TASK_MAX_COUNT];
static uint8_t task_stacks[TASK_MAX_COUNT-1u][TASK_STACK_SIZE] __attribute__((aligned(16)));
static uint32_t current_task;
static uint8_t tasks_ready;
static volatile uint64_t test_progress[2];
static uint64_t interrupt_lock(void) {
uint64_t flags;
__asm__ volatile("pushfq; popq %0; cli":"=r"(flags)::"memory");
return flags;
}
static void interrupt_restore(uint64_t flags) {
__asm__ volatile("pushq %0; popfq"::"r"(flags):"memory","cc");
}
static void copy_context(struct task_context *destination,const struct task_context *source) {
volatile uint64_t *to=(volatile uint64_t *)destination;
const volatile uint64_t *from=(const volatile uint64_t *)source;
for(uint32_t index=0;index<TASK_CONTEXT_WORDS;++index) {
to[index]=from[index];
}
}
__attribute__((used)) static void task_finished(void) {
task_slots[current_task].runnable=0;
}
__attribute__((naked,noreturn,used)) static void task_return_stub(void) {
__asm__ volatile(
"cli\n"
"call task_finished\n"
"sti\n"
"1: hlt\n"
"jmp 1b\n"
);
}
int task_create(void (*entry)(void)) {
if(!tasks_ready||entry==(void (*)(void))0) {
return -1;
}
uint64_t flags=interrupt_lock();
uint32_t identifier;
for(identifier=1;identifier<TASK_MAX_COUNT;++identifier) {
if(!task_slots[identifier].runnable) {
break;
}
}
if(identifier==TASK_MAX_COUNT) {
interrupt_restore(flags);
return -1;
}
struct task_context *context=&task_slots[identifier].context;
for(uint32_t index=0;index<TASK_CONTEXT_WORDS;++index) {
((uint64_t *)context)[index]=0;
}
uintptr_t stack=(uintptr_t)(task_stacks[identifier-1u]+TASK_STACK_SIZE);
stack&=~(uintptr_t)15u;
stack-=sizeof(uint64_t);
*(uint64_t *)stack=(uint64_t)(uintptr_t)task_return_stub;
context->rip=(uint64_t)(uintptr_t)entry;
context->cs=KERNEL_CODE_SELECTOR;
context->rflags=0x202u;
context->rsp=stack;
context->ss=KERNEL_DATA_SELECTOR;
task_slots[identifier].runnable=1;
interrupt_restore(flags);
return (int)identifier;
}
int task_destroy(uint32_t identifier) {
uint64_t flags=interrupt_lock();
int valid=tasks_ready&&identifier>0&&identifier<TASK_MAX_COUNT&&
identifier!=current_task&&task_slots[identifier].runnable;
if(valid) {
task_slots[identifier].runnable=0;
}
interrupt_restore(flags);
return valid;
}
struct task_context *tasks_schedule(struct task_context *frame) {
if(!tasks_ready) {
return frame;
}
uint32_t previous=current_task;
if(task_slots[previous].runnable) {
copy_context(&task_slots[previous].context,frame);
}
uint32_t next=previous;
for(uint32_t distance=1;distance<=TASK_MAX_COUNT;++distance) {
uint32_t candidate=(previous+distance)%TASK_MAX_COUNT;
if(task_slots[candidate].runnable) {
next=candidate;
break;
}
}
current_task=next;
return &task_slots[next].context;
}
static void test_worker_zero(void) {
for(;;) {
++test_progress[0];
}
}
static void test_worker_one(void) {
for(;;) {
++test_progress[1];
}
}
int tasks_init(void) {
uint64_t flags=interrupt_lock();
for(uint32_t index=0;index<TASK_MAX_COUNT;++index) {
task_slots[index].runnable=0;
}
current_task=0;
task_slots[0].runnable=1;
tasks_ready=1;
test_progress[0]=0;
test_progress[1]=0;
interrupt_restore(flags);
int first=task_create(test_worker_zero);
int second=task_create(test_worker_one);
if(first<0||second<0) {
if(first>0) {
(void)task_destroy((uint32_t)first);
}
return 0;
}
uint64_t start=timer_ticks();
while((test_progress[0]==0||test_progress[1]==0)&&
timer_ticks()-start<TIMER_FREQUENCY_HZ) {
__asm__ volatile("pause");
}
int success=test_progress[0]!=0&&test_progress[1]!=0;
(void)task_destroy((uint32_t)first);
(void)task_destroy((uint32_t)second);
return success;
}
