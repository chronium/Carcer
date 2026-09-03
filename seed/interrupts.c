#include "interrupts.h"
#include "tasks.h"
typedef unsigned char u8;typedef unsigned u32;typedef unsigned long u64;
#define IDTN 256u
#define IG 0x8eu
#define UIG 0xeeu
#define SYSV 0x80u
#define KCS 0x08u
#define ISS (16u * 1024u)
#define ISTI 1u
#define PMC 0x20u
#define PMD 0x21u
#define PSC 0xa0u
#define PSD 0xa1u
#define PI 0x11u
#define P86 0x01u
#define PMV 0x20u
#define PSV 0x28u
#define PTV PMV
#define PCZ 0x40u
#define PC 0x43u
#define PRG 0x34u
#define PHZ 1193182u
#define TST 2u
struct ie{uint16_t offset_low;uint16_t selector;u8 ist;u8 attributes;uint16_t offset_middle;u32 offset_high;u32 reserved;}__attribute__((packed));struct dtp{uint16_t limit;u64 base;}__attribute__((packed));struct tseg{u32 reserved0;u64 rsp[3];u64 reserved1;u64 ist[7];u64 reserved2;uint16_t reserved3;uint16_t io_map_base;}__attribute__((packed));static struct ie idt[IDTN];static u64 gdt[7];static struct tseg tss;static u8 istack[ISS]__attribute__((aligned(16)));volatile u64 ticks;static void outb(uint16_t port,u8 value){__asm__ volatile("outb %0, %1"::"a"(value),"Nd"(port));}static void io_wait(void){outb(0x80u,0);}__attribute__((naked,noreturn,used))static void fatal_stub(void){__asm__ volatile("cli\n""1: hlt\n""jmp 1b\n");}
#define SAVE "pushq %rax\n""pushq %rbx\n""pushq %rcx\n""pushq %rdx\n""pushq %rbp\n""pushq %rsi\n""pushq %rdi\n""pushq %r8\n""pushq %r9\n""pushq %r10\n""pushq %r11\n""pushq %r12\n""pushq %r13\n""pushq %r14\n""pushq %r15\n""cld\n"
#define RESTORE "popq %r15\n""popq %r14\n""popq %r13\n""popq %r12\n""popq %r11\n""popq %r10\n""popq %r9\n""popq %r8\n""popq %rdi\n""popq %rsi\n""popq %rbp\n""popq %rdx\n""popq %rcx\n""popq %rbx\n""popq %rax\n""iretq\n"
__attribute__((used))static struct task_context*timer(struct task_context*frame){++ticks;outb(PMC,0x20u);return task_sched(frame,1);}__attribute__((used))static struct task_context*syscall(struct task_context*frame){return task_syscall(frame);}__attribute__((used))static struct task_context*except(struct task_context*frame){if((frame->cs&3u)==3u){frame->rax=0;frame->rdi=UINT64_MAX;return task_syscall(frame);}fatal_stub();}
__attribute__((naked,used))static void exstub(void){__asm__ volatile("pushq $0\n""jmp excommon\n");}__attribute__((naked,used))static void exerr(void){__asm__ volatile("jmp excommon\n");}__attribute__((naked,used))static void excommon(void){__asm__ volatile("addq $8,%rsp\n"SAVE"movq %rsp,%rdi\n""call except\n""movq %rax,%rsp\n"RESTORE);}
__attribute__((naked,used))static void timerstub(void){__asm__ volatile(SAVE"movq %rsp,%rdi\n""call timer\n""movq %rax,%rsp\n"RESTORE);}__attribute__((naked,used))static void sysstub(void){__asm__ volatile(SAVE"movq %rsp,%rdi\n""call syscall\n""movq %rax,%rsp\n"RESTORE);}static uint16_t codesel(void){uint16_t selector;__asm__ volatile("movw %%cs, %0":"=r"(selector));return selector;}static int gdt_init(void){u64 tss_base=(u64)(uintptr_t)&tss;u64 tss_limit=sizeof(tss)-1u;tss.ist[0]=tss.ist[1]=(u64)(uintptr_t)(istack+sizeof(istack));tss.io_map_base=sizeof(tss);gdt[0]=0;gdt[1]=0x00af9a000000ffffull;gdt[2]=0x00cf92000000ffffull;gdt[3]=0x00cff2000000ffffull;gdt[4]=0x00affa000000ffffull;gdt[5]=(tss_limit&0xffffu)|((tss_base&0xffffffu)<<16)|(0x89ull<<40)|(((tss_limit>>16)&0x0fu)<<48)|(((tss_base>>24)&0xffu)<<56);gdt[6]=tss_base>>32;struct dtp descriptor={(uint16_t)(sizeof(gdt)-1u),(u64)(uintptr_t)gdt};__asm__ volatile("lgdt %0\n""pushq $0x08\n""leaq 1f(%%rip), %%rax\n""pushq %%rax\n""lretq\n""1:\n""movw $0x10, %%ax\n""movw %%ax, %%ds\n""movw %%ax, %%es\n""movw %%ax, %%ss\n""movw $0x28, %%ax\n""ltr %%ax\n"::"m"(descriptor):"rax","memory");return codesel()==KCS;}static void idt_set(u8 vector,void(*handler)(void),u8 attributes){u64 address=(u64)(uintptr_t)handler;struct ie*entry=&idt[vector];entry->offset_low=(uint16_t)address;entry->selector=KCS;entry->ist=ISTI;entry->attributes=attributes;entry->offset_middle=(uint16_t)(address>>16);entry->offset_high=(u32)(address>>32);entry->reserved=0;}static void idt_load(void){struct dtp descriptor={(uint16_t)(sizeof(idt)-1u),(u64)(uintptr_t)idt};__asm__ volatile("lidt %0"::"m"(descriptor):"memory");}static void pic_init(void){outb(PMC,PI);io_wait();outb(PSC,PI);io_wait();outb(PMD,PMV);io_wait();outb(PSD,PSV);io_wait();outb(PMD,1u<<2);io_wait();outb(PSD,2u);io_wait();outb(PMD,P86);io_wait();outb(PSD,P86);io_wait();outb(PMD,0xfeu);outb(PSD,0xffu);}static void pit_init(void){uint16_t divisor=(uint16_t)(PHZ/TIMER_FREQUENCY_HZ);outb(PC,PRG);outb(PCZ,(u8)divisor);outb(PCZ,(u8)(divisor>>8));}int interrupts_init(void){__asm__ volatile("cli":::"memory");if(!gdt_init()){return 0;}for(uint16_t vector=0;vector<IDTN;++vector)idt_set((u8)vector,fatal_stub,IG);for(u8 vector=0;vector<32u;++vector)idt_set(vector,exstub,IG);static const u8 errors[]={8,10,11,12,13,14,17,21,29,30};for(u8 i=0;i<sizeof(errors);++i)idt_set(errors[i],exerr,IG);idt_set(PTV,timerstub,IG);idt[PTV].ist=2;idt_set(SYSV,sysstub,UIG);idt_load();ticks=0;pic_init();pit_init();__asm__ volatile("sti":::"memory");u64 start=ticks;while(ticks-start<TST){__asm__ volatile("hlt");}return 1;}void interrupts_set_task_stack(void*p){tss.ist[0]=(u64)(uintptr_t)p;}u64 timer_ticks(void){return ticks;}
