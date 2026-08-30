#include "tasks.h"
#include "interrupts.h"
#include "memory.h"
#define TASK_MAX_COUNT 4u
#define TASK_STACK_SIZE (16u*1024u)
#define TASK_CONTEXT_WORDS (sizeof(struct task_context)/sizeof(uint64_t))
#define KERNEL_CODE_SELECTOR 0x08u
#define KERNEL_DATA_SELECTOR 0x10u
#define USER_DATA_SELECTOR 0x1bu
#define USER_CODE_SELECTOR 0x23u
#define USER_IMAGE_BASE 0x400000ull
#define USER_STACK_TOP 0x600000ull
#define USER_ADDRESS_SPACE_PAGES 6u
#define PAGE_ADDRESS_MASK 0x000ffffffffff000ull
#define PAGE_USER_RW 7ull
struct task_slot {
struct task_context context;
uint64_t cr3;
uint64_t pages;
uint8_t runnable;
};
static struct task_slot task_slots[TASK_MAX_COUNT];
static uint8_t task_stacks[TASK_MAX_COUNT-1u][TASK_STACK_SIZE] __attribute__((aligned(16)));
static uint32_t current_task;
static uint8_t tasks_ready;
static uint64_t interrupt_lock(void) {
uint64_t flags;
__asm__ volatile("pushfq; popq %0; cli":"=r"(flags)::"memory");
return flags;
}
static void interrupt_restore(uint64_t flags) {
__asm__ volatile("pushq %0; popfq"::"r"(flags):"memory","cc");
}
static uint64_t read_cr3(void) {
uint64_t value;
__asm__ volatile("movq %%cr3,%0":"=r"(value));
return value;
}
static void write_cr3(uint64_t value) {
__asm__ volatile("movq %0,%%cr3"::"r"(value):"memory");
}
static void copy_context(struct task_context *destination,const struct task_context *source) {
volatile uint64_t *to=(volatile uint64_t *)destination;
const volatile uint64_t *from=(const volatile uint64_t *)source;
for(uint32_t index=0;index<TASK_CONTEXT_WORDS;++index) to[index]=from[index];
}
static int free_slot(void) {
for(uint32_t identifier=1;identifier<TASK_MAX_COUNT;++identifier)
if(!task_slots[identifier].runnable) return (int)identifier;
return -1;
}
static void clear_context(struct task_context *context) {
for(uint32_t index=0;index<TASK_CONTEXT_WORDS;++index)
((uint64_t *)context)[index]=0;
}
__attribute__((used)) static void task_finished(void) {
task_slots[current_task].runnable=0;
}
__attribute__((naked,noreturn,used)) static void task_return_stub(void) {
__asm__ volatile("cli\ncall task_finished\nsti\n1: hlt\njmp 1b\n");
}
int task_create(void (*entry)(void)) {
if(!tasks_ready||entry==(void (*)(void))0) return -1;
uint64_t flags=interrupt_lock();
int identifier=free_slot();
if(identifier<0) {
interrupt_restore(flags);
return -1;
}
struct task_slot *slot=&task_slots[identifier];
clear_context(&slot->context);
uintptr_t stack=(uintptr_t)(task_stacks[identifier-1]+TASK_STACK_SIZE);
stack=(stack&~(uintptr_t)15u)-sizeof(uint64_t);
*(uint64_t *)stack=(uint64_t)(uintptr_t)task_return_stub;
slot->context.rip=(uint64_t)(uintptr_t)entry;
slot->context.cs=KERNEL_CODE_SELECTOR;
slot->context.rflags=0x202u;
slot->context.rsp=stack;
slot->context.ss=KERNEL_DATA_SELECTOR;
slot->cr3=task_slots[0].cr3;
slot->pages=0;
slot->runnable=1;
interrupt_restore(flags);
return identifier;
}
static uint64_t create_user_space(const uint8_t *image,uint32_t size) {
uint64_t base=memory_pages_alloc(USER_ADDRESS_SPACE_PAGES);
if(!base) return 0;
uint64_t *root=(uint64_t *)memory_physical_to_virtual(base);
uint64_t *kernel_root=(uint64_t *)memory_physical_to_virtual(task_slots[0].cr3&PAGE_ADDRESS_MASK);
uint64_t *level3=(uint64_t *)memory_physical_to_virtual(base+MEMORY_PAGE_SIZE);
uint64_t *level2=(uint64_t *)memory_physical_to_virtual(base+2u*MEMORY_PAGE_SIZE);
uint64_t *level1=(uint64_t *)memory_physical_to_virtual(base+3u*MEMORY_PAGE_SIZE);
uint8_t *user_image=(uint8_t *)memory_physical_to_virtual(base+4u*MEMORY_PAGE_SIZE);
if(!root||!kernel_root||!level3||!level2||!level1||!user_image) {
(void)memory_pages_free(base,USER_ADDRESS_SPACE_PAGES);
return 0;
}
for(uint32_t index=0;index<512u;++index) root[index]=kernel_root[index];
root[0]=(base+MEMORY_PAGE_SIZE)|PAGE_USER_RW;
level3[0]=(base+2u*MEMORY_PAGE_SIZE)|PAGE_USER_RW;
level2[2]=(base+3u*MEMORY_PAGE_SIZE)|PAGE_USER_RW;
level1[0]=(base+4u*MEMORY_PAGE_SIZE)|PAGE_USER_RW;
level1[511]=(base+5u*MEMORY_PAGE_SIZE)|PAGE_USER_RW;
for(uint32_t index=0;index<size;++index) user_image[index]=image[index];
return base;
}
int task_create_user(const uint8_t *image,uint32_t size,uint32_t entry_offset) {
if(!tasks_ready||!image||!size||size>MEMORY_PAGE_SIZE||entry_offset>=size) return -1;
uint64_t flags=interrupt_lock();
int identifier=free_slot();
if(identifier<0) {
interrupt_restore(flags);
return -1;
}
uint64_t pages=create_user_space(image,size);
if(!pages) {
interrupt_restore(flags);
return -1;
}
struct task_slot *slot=&task_slots[identifier];
clear_context(&slot->context);
slot->context.rip=USER_IMAGE_BASE+entry_offset;
slot->context.cs=USER_CODE_SELECTOR;
slot->context.rflags=0x202u;
slot->context.rsp=USER_STACK_TOP;
slot->context.ss=USER_DATA_SELECTOR;
slot->cr3=pages;
slot->pages=pages;
slot->runnable=1;
interrupt_restore(flags);
return identifier;
}
int task_destroy(uint32_t identifier) {
uint64_t flags=interrupt_lock();
int valid=tasks_ready&&identifier>0&&identifier<TASK_MAX_COUNT&&
identifier!=current_task&&task_slots[identifier].runnable;
if(valid&&task_slots[identifier].pages)
valid=memory_pages_free(task_slots[identifier].pages,USER_ADDRESS_SPACE_PAGES);
if(valid) {
task_slots[identifier].runnable=0;
task_slots[identifier].pages=0;
}
interrupt_restore(flags);
return valid;
}
struct task_context *tasks_schedule(struct task_context *frame) {
if(!tasks_ready) return frame;
uint32_t previous=current_task;
if(task_slots[previous].runnable) copy_context(&task_slots[previous].context,frame);
uint32_t next=previous;
for(uint32_t distance=1;distance<=TASK_MAX_COUNT;++distance) {
uint32_t candidate=(previous+distance)%TASK_MAX_COUNT;
if(task_slots[candidate].runnable) {
next=candidate;
break;
}
}
current_task=next;
if(task_slots[next].cr3!=read_cr3()) write_cr3(task_slots[next].cr3);
return &task_slots[next].context;
}
static const uint8_t worker_up[]={
0x48,0xb8,0x00,0x01,0x40,0,0,0,0,0,0x48,0xff,0x00,0xeb,0xfb
};
static const uint8_t worker_down[]={
0x48,0xb8,0x00,0x01,0x40,0,0,0,0,0,0x48,0xff,0x08,0xeb,0xfb
};
int tasks_init(void) {
uint64_t flags=interrupt_lock();
for(uint32_t index=0;index<TASK_MAX_COUNT;++index) {
task_slots[index].runnable=0;
task_slots[index].pages=0;
}
current_task=0;
task_slots[0].cr3=read_cr3();
task_slots[0].runnable=1;
tasks_ready=1;
interrupt_restore(flags);
int first=task_create_user(worker_up,sizeof(worker_up),0);
int second=task_create_user(worker_down,sizeof(worker_down),0);
if(first<0||second<0) {
if(first>0) (void)task_destroy((uint32_t)first);
return 0;
}
volatile uint64_t *a=(volatile uint64_t *)((uint8_t *)memory_physical_to_virtual(task_slots[first].pages+4u*MEMORY_PAGE_SIZE)+0x100u);
volatile uint64_t *b=(volatile uint64_t *)((uint8_t *)memory_physical_to_virtual(task_slots[second].pages+4u*MEMORY_PAGE_SIZE)+0x100u);
uint64_t start=timer_ticks();
while((*a==0||*b==0)&&timer_ticks()-start<TIMER_FREQUENCY_HZ)
__asm__ volatile("pause");
int success=*a!=0&&*b!=0;
(void)task_destroy((uint32_t)first);
(void)task_destroy((uint32_t)second);
return success;
}
