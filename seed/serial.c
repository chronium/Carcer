#include "serial.h"

#define COM1 0x3f8u

static void outb(uint16_t port, uint8_t value) {
    __asm__ volatile("outb %0, %1" : : "a"(value), "Nd"(port));
}

static uint8_t inb(uint16_t port) {
    uint8_t value;
    __asm__ volatile("inb %1, %0" : "=a"(value) : "Nd"(port));
    return value;
}

void serial_init(void) {
    outb(COM1 + 1, 0x00);
    outb(COM1 + 3, 0x80);
    outb(COM1 + 0, 0x03);
    outb(COM1 + 1, 0x00);
    outb(COM1 + 3, 0x03);
    outb(COM1 + 2, 0xc7);
    outb(COM1 + 4, 0x0b);
}

void serial_write(uint8_t byte) {
    while ((inb(COM1 + 5) & 0x20) == 0) {
    }
    outb(COM1, byte);
}

uint8_t serial_read(void) {
    while ((inb(COM1 + 5) & 0x01) == 0) {
    }
    return inb(COM1);
}

void serial_write_bytes(const uint8_t *data, uint32_t length) {
    for (uint32_t index = 0; index < length; ++index) {
        serial_write(data[index]);
    }
}
