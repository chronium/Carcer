#pragma GCC target("general-regs-only")
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
struct ie{uint16_t lo;uint16_t s;u8 ist;u8 q;uint16_t mi;u32 hi;u32 r;}__attribute__((packed));struct dtp{uint16_t limit;u64 base;}__attribute__((packed));struct tseg{u32 reserved0;u64 rsp[3];u64 reserved1;u64 ist[7];u64 reserved2;uint16_t reserved3;uint16_t io_map_base;}__attribute__((packed));static struct ie idt[IDTN];static u64 gdt[7];static struct tseg tss;static u8 istack[ISS]__attribute__((aligned(16)));const u8 fxclean[512]__attribute__((aligned(16)))={[0]=0x7f,[1]=3,[24]=0x80,[25]=0x1f};
__attribute__((naked,noinline,used))void fxreset(void){__asm__ volatile("fnclex\nemms\n.global fxsan\nfxsan: fildl fxclean(%rip)\nret\n");}
int fxinit(void){u32 a,b,c,d;__asm__ volatile("cpuid":"=a"(a),"=b"(b),"=c"(c),"=d"(d):"a"(1),"c"(0));if((d&0x07000001u)!=0x07000001u)return 0;u64 q;__asm__ volatile("mov %%cr0,%0":"=r"(q));q=(q&~12ull)|34ull;__asm__ volatile("mov %0,%%cr0"::"r"(q):"memory");__asm__ volatile("mov %%cr4,%0":"=r"(q));q=(q&~((1ull<<16)|(1ull<<18)|(1ull<<22)|(1ull<<23)|(1ull<<24)|(1ull<<25)))|0x600ull;__asm__ volatile("mov %0,%%cr4"::"r"(q):"memory");fxreset();__asm__ volatile("fxrstor64 %0"::"m"(fxclean):"memory");return 1;}
volatile u64 ticks;static void outb(uint16_t port,u8 value){__asm__ volatile("outb %0, %1"::"a"(value),"Nd"(port));}static void iw(void){outb(0x80u,0);}__attribute__((naked,noreturn,used))static void fs(void){__asm__ volatile("cli\n""1: hlt\n""jmp 1b\n");}
#define SAVE "pushq %rax\n""pushq %rbx\n""pushq %rcx\n""pushq %rdx\n""pushq %rbp\n""pushq %rsi\n""pushq %rdi\n""pushq %r8\n""pushq %r9\n""pushq %r10\n""pushq %r11\n""pushq %r12\n""pushq %r13\n""pushq %r14\n""pushq %r15\n""cld\n""subq $512,%rsp\n""fxsave64 (%rsp)\n""call fxreset\n""fxrstor64 fxclean(%rip)\n"
#define RESTORE "fxrstor64 (%rsp)\n""addq $512,%rsp\n""popq %r15\n""popq %r14\n""popq %r13\n""popq %r12\n""popq %r11\n""popq %r10\n""popq %r9\n""popq %r8\n""popq %rdi\n""popq %rsi\n""popq %rbp\n""popq %rdx\n""popq %rcx\n""popq %rbx\n""popq %rax\n""iretq\n"
__attribute__((used))static struct tc*timer(struct tc*frame){++ticks;outb(PMC,0x20u);return tsched(frame,1);}__attribute__((used))static struct tc*syscall(struct tc*frame){return tsys(frame);}__attribute__((used))static struct tc*except(struct tc*frame){if((frame->cs&3u)==3u){frame->rax=0;frame->rdi=UINT64_MAX;return tsys(frame);}fs();}
__attribute__((naked,used))static void xstub(void){__asm__ volatile("pushq $0\n""jmp xc\n");}__attribute__((naked,used))static void xerr(void){__asm__ volatile("jmp xc\n");}__attribute__((naked,used))static void xc(void){__asm__ volatile("addq $8,%rsp\n"SAVE"movq %rsp,%rdi\n""call except\n""call fxreset\n""movq %rax,%rsp\n"RESTORE);}
__attribute__((naked,used))static void tstub(void){__asm__ volatile(SAVE"movq %rsp,%rdi\n""call timer\n""call fxreset\n""movq %rax,%rsp\n"RESTORE);}__attribute__((naked,used))static void sstub(void){__asm__ volatile(SAVE"movq %rsp,%rdi\n""call syscall\n""call fxreset\n""movq %rax,%rsp\n"RESTORE);}static uint16_t csel(void){uint16_t s;__asm__ volatile("movw %%cs, %0":"=r"(s));return s;}static int gi(void){u64 tss_base=(u64)(uintptr_t)&tss;u64 tss_limit=sizeof(tss)-1u;tss.ist[0]=tss.ist[1]=(u64)(uintptr_t)(istack+sizeof(istack));tss.io_map_base=sizeof(tss);gdt[0]=0;gdt[1]=0x00af9a000000ffffull;gdt[2]=0x00cf92000000ffffull;gdt[3]=0x00cff2000000ffffull;gdt[4]=0x00affa000000ffffull;gdt[5]=(tss_limit&0xffffu)|((tss_base&0xffffffu)<<16)|(0x89ull<<40)|(((tss_limit>>16)&0x0fu)<<48)|(((tss_base>>24)&0xffu)<<56);gdt[6]=tss_base>>32;struct dtp d={(uint16_t)(sizeof(gdt)-1u),(u64)(uintptr_t)gdt};__asm__ volatile("lgdt %0\n""pushq $0x08\n""leaq 1f(%%rip), %%rax\n""pushq %%rax\n""lretq\n""1:\n""movw $0x10, %%ax\n""movw %%ax, %%ds\n""movw %%ax, %%es\n""movw %%ax, %%ss\n""movw $0x28, %%ax\n""ltr %%ax\n"::"m"(d):"rax","memory");return csel()==KCS;}static void iset(u8 v,void(*h)(void),u8 q){u64 a=(u64)(uintptr_t)h;struct ie*entry=&idt[v];entry->lo=(uint16_t)a;entry->s=KCS;entry->ist=ISTI;entry->q=q;entry->mi=(uint16_t)(a>>16);entry->hi=(u32)(a>>32);entry->r=0;}static void iload(void){struct dtp d={(uint16_t)(sizeof(idt)-1u),(u64)(uintptr_t)idt};__asm__ volatile("lidt %0"::"m"(d):"memory");}static void pinit(void){outb(PMC,PI);iw();outb(PSC,PI);iw();outb(PMD,PMV);iw();outb(PSD,PSV);iw();outb(PMD,1u<<2);iw();outb(PSD,2u);iw();outb(PMD,P86);iw();outb(PSD,P86);iw();outb(PMD,0xfeu);outb(PSD,0xffu);}static void tinit0(void){uint16_t divisor=(uint16_t)(PHZ/THZ);outb(PC,PRG);outb(PCZ,(u8)divisor);outb(PCZ,(u8)(divisor>>8));}int iinit(void){__asm__ volatile("cli":::"memory");if(!gi()){return 0;}for(uint16_t v=0;v<IDTN;++v)iset((u8)v,fs,IG);for(u8 v=0;v<32u;++v)iset(v,xstub,IG);static const u8 errors[]={8,10,11,12,13,14,17,21,29,30};for(u8 i=0;i<sizeof(errors);++i)iset(errors[i],xerr,IG);iset(PTV,tstub,IG);idt[PTV].ist=2;iset(SYSV,sstub,UIG);iload();ticks=0;pinit();tinit0();__asm__ volatile("sti":::"memory");u64 start=ticks;while(ticks-start<TST){__asm__ volatile("hlt");}return 1;}void itstack(void*p){tss.ist[0]=(u64)(uintptr_t)p;}u64 tnow(void){return ticks;}
