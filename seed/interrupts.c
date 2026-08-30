#include "interrupts.h"
#define IDT_ENTRY_COUNT 256u
#define IDT_INTERRUPT_GATE 0x8eu
#define KERNEL_CODE_SELECTOR 0x08u
#define INTERRUPT_STACK_SIZE (16u * 1024u)
#define INTERRUPT_STACK_TABLE_INDEX 1u
#define PIC_MASTER_COMMAND 0x20u
#define PIC_MASTER_DATA 0x21u
#define PIC_SLAVE_COMMAND 0xa0u
#define PIC_SLAVE_DATA 0xa1u
#define PIC_INITIALIZE 0x11u
#define PIC_8086_MODE 0x01u
#define PIC_MASTER_VECTOR_BASE 0x20u
#define PIC_SLAVE_VECTOR_BASE 0x28u
#define PIC_TIMER_VECTOR PIC_MASTER_VECTOR_BASE
#define PIT_CHANNEL_ZERO 0x40u
#define PIT_COMMAND 0x43u
#define PIT_RATE_GENERATOR 0x34u
#define PIT_INPUT_HZ 1193182u
#define TIMER_SELF_TEST_TICKS 2u
struct idt_entry {
 uint16_t offset_low;
 uint16_t selector;
 uint8_t ist;
 uint8_t attributes;
 uint16_t offset_middle;
 uint32_t offset_high;
 uint32_t reserved;
} __attribute__((packed));
struct descriptor_table_pointer {
 uint16_t limit;
 uint64_t base;
} __attribute__((packed));
struct task_state_segment {
 uint32_t reserved0;
 uint64_t rsp[3];
 uint64_t reserved1;
 uint64_t ist[7];
 uint64_t reserved2;
 uint16_t reserved3;
 uint16_t io_map_base;
} __attribute__((packed));
static struct idt_entry idt[IDT_ENTRY_COUNT];
static uint64_t gdt[5];
static struct task_state_segment tss;
static uint8_t interrupt_stack[INTERRUPT_STACK_SIZE] __attribute__((aligned(16)));
volatile uint64_t kernel_timer_ticks;
static void outb(uint16_t port, uint8_t value) {
 __asm__ volatile("outb %0, %1" : : "a"(value), "Nd"(port));
}
static void io_wait(void) {
 outb(0x80u, 0);
}
__attribute__((naked, used)) static void interrupt_fatal_stub(void) {
 __asm__ volatile(
 "cli\n"
 "1: hlt\n"
 "jmp 1b\n"
 );
}
__attribute__((naked, used)) static void timer_interrupt_stub(void) {
 __asm__ volatile(
 "pushq %rax\n"
 "incq kernel_timer_ticks(%rip)\n"
 "movb $0x20, %al\n"
 "outb %al, $0x20\n"
 "popq %rax\n"
 "iretq\n"
 );
}
static uint16_t current_code_selector(void) {
 uint16_t selector;
 __asm__ volatile("movw %%cs, %0" : "=r"(selector));
 return selector;
}
static int initialize_gdt(void) {
 uint64_t tss_base = (uint64_t)(uintptr_t)&tss;
 uint64_t tss_limit = sizeof(tss) - 1u;
 tss.ist[0] = (uint64_t)(uintptr_t)(interrupt_stack + sizeof(interrupt_stack));
 tss.io_map_base = sizeof(tss);
 gdt[0] = 0;
 gdt[1] = 0x00af9a000000ffffull;
 gdt[2] = 0x00cf92000000ffffull;
 gdt[3] = (tss_limit & 0xffffu) |
     ((tss_base & 0xffffffu) << 16) |
     (0x89ull << 40) |
     (((tss_limit >> 16) & 0x0fu) << 48) |
     (((tss_base >> 24) & 0xffu) << 56);
 gdt[4] = tss_base >> 32;
 struct descriptor_table_pointer descriptor = {
 (uint16_t)(sizeof(gdt) - 1u),
 (uint64_t)(uintptr_t)gdt
 };
 __asm__ volatile(
 "lgdt %0\n"
 "pushq $0x08\n"
 "leaq 1f(%%rip), %%rax\n"
 "pushq %%rax\n"
 "lretq\n"
 "1:\n"
 "movw $0x10, %%ax\n"
 "movw %%ax, %%ds\n"
 "movw %%ax, %%es\n"
 "movw %%ax, %%ss\n"
 "movw $0x18, %%ax\n"
 "ltr %%ax\n"
 : : "m"(descriptor) : "rax", "memory"
 );
 return current_code_selector() == KERNEL_CODE_SELECTOR;
}
static void set_idt_entry(uint8_t vector, void (*handler)(void)) {
 uint64_t address = (uint64_t)(uintptr_t)handler;
 struct idt_entry *entry = &idt[vector];
 entry->offset_low = (uint16_t)address;
 entry->selector = KERNEL_CODE_SELECTOR;
 entry->ist = INTERRUPT_STACK_TABLE_INDEX;
 entry->attributes = IDT_INTERRUPT_GATE;
 entry->offset_middle = (uint16_t)(address >> 16);
 entry->offset_high = (uint32_t)(address >> 32);
 entry->reserved = 0;
}
static void load_idt(void) {
 struct descriptor_table_pointer descriptor = {
 (uint16_t)(sizeof(idt) - 1u),
 (uint64_t)(uintptr_t)idt
 };
 __asm__ volatile("lidt %0" : : "m"(descriptor) : "memory");
}
static void initialize_pic(void) {
 outb(PIC_MASTER_COMMAND, PIC_INITIALIZE);
 io_wait();
 outb(PIC_SLAVE_COMMAND, PIC_INITIALIZE);
 io_wait();
 outb(PIC_MASTER_DATA, PIC_MASTER_VECTOR_BASE);
 io_wait();
 outb(PIC_SLAVE_DATA, PIC_SLAVE_VECTOR_BASE);
 io_wait();
 outb(PIC_MASTER_DATA, 1u << 2);
 io_wait();
 outb(PIC_SLAVE_DATA, 2u);
 io_wait();
 outb(PIC_MASTER_DATA, PIC_8086_MODE);
 io_wait();
 outb(PIC_SLAVE_DATA, PIC_8086_MODE);
 io_wait();
 outb(PIC_MASTER_DATA, 0xfeu);
 outb(PIC_SLAVE_DATA, 0xffu);
}
static void initialize_pit(void) {
 uint16_t divisor = (uint16_t)(PIT_INPUT_HZ / TIMER_FREQUENCY_HZ);
 outb(PIT_COMMAND, PIT_RATE_GENERATOR);
 outb(PIT_CHANNEL_ZERO, (uint8_t)divisor);
 outb(PIT_CHANNEL_ZERO, (uint8_t)(divisor >> 8));
}
int interrupts_init(void) {
 __asm__ volatile("cli" ::: "memory");
 if (!initialize_gdt()) {
 return 0;
 }
 for (uint16_t vector = 0; vector < IDT_ENTRY_COUNT; ++vector) {
 set_idt_entry((uint8_t)vector, interrupt_fatal_stub);
 }
 set_idt_entry(PIC_TIMER_VECTOR, timer_interrupt_stub);
 load_idt();
 kernel_timer_ticks = 0;
 initialize_pic();
 initialize_pit();
 __asm__ volatile("sti" ::: "memory");
 uint64_t start = kernel_timer_ticks;
 while (kernel_timer_ticks - start < TIMER_SELF_TEST_TICKS) {
 __asm__ volatile("hlt");
 }
 return 1;
}
uint64_t timer_ticks(void) {
 return kernel_timer_ticks;
}
