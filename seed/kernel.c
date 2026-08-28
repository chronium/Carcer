#include <stdint.h>

#define COM1 0x3f8

static void outb(uint16_t port, uint8_t value) {
    __asm__ volatile("outb %0, %1" : : "a"(value), "Nd"(port));
}

static uint8_t inb(uint16_t port) {
    uint8_t value;
    __asm__ volatile("inb %1, %0" : "=a"(value) : "Nd"(port));
    return value;
}

static void serial_init(void) {
    outb(COM1 + 1, 0x00);
    outb(COM1 + 3, 0x80);
    outb(COM1 + 0, 0x03);
    outb(COM1 + 1, 0x00);
    outb(COM1 + 3, 0x03);
    outb(COM1 + 2, 0xc7);
    outb(COM1 + 4, 0x0b);
}

static void serial_write(uint8_t byte) {
    while ((inb(COM1 + 5) & 0x20) == 0) {
    }
    outb(COM1, byte);
}

__attribute__((noreturn)) void kmain(void) {
    static const char ready[] = "CODEXOS-SEED-READY\n";

    serial_init();
    for (uint64_t index = 0; index < sizeof(ready) - 1; ++index) {
        serial_write((uint8_t)ready[index]);
    }

    for (;;) {
        __asm__ volatile("hlt");
    }
}
