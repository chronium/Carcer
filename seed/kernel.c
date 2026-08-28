#include <stdint.h>

#define COM1 0x3f8u
#define FRAME_HEADER_SIZE 16u
#define FRAME_MAX_PAYLOAD (16u * 1024u * 1024u)
#define FRAME_PROTOCOL_VERSION 1u
#define LIST_TOOLS_REQUEST 0x0001u
#define LIST_TOOLS_RESPONSE 0x8001u

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

static uint8_t serial_read(void) {
    while ((inb(COM1 + 5) & 0x01) == 0) {
    }
    return inb(COM1);
}

static uint16_t read_u16_le(const uint8_t *bytes) {
    return (uint16_t)bytes[0] | ((uint16_t)bytes[1] << 8);
}

static uint32_t read_u32_le(const uint8_t *bytes) {
    return (uint32_t)bytes[0] | ((uint32_t)bytes[1] << 8) |
           ((uint32_t)bytes[2] << 16) | ((uint32_t)bytes[3] << 24);
}

static void write_u16_le(uint8_t *bytes, uint16_t value) {
    bytes[0] = (uint8_t)value;
    bytes[1] = (uint8_t)(value >> 8);
}

static void write_u32_le(uint8_t *bytes, uint32_t value) {
    bytes[0] = (uint8_t)value;
    bytes[1] = (uint8_t)(value >> 8);
    bytes[2] = (uint8_t)(value >> 16);
    bytes[3] = (uint8_t)(value >> 24);
}

static void discard_bytes(uint32_t count) {
    for (uint32_t index = 0; index < count; ++index) {
        (void)serial_read();
    }
}

static void send_empty_tool_list(uint32_t request_id) {
    uint8_t response[FRAME_HEADER_SIZE + 2];

    response[0] = 'C';
    response[1] = 'X';
    response[2] = 'O';
    response[3] = 'S';
    write_u16_le(response + 4, FRAME_PROTOCOL_VERSION);
    write_u16_le(response + 6, LIST_TOOLS_RESPONSE);
    write_u32_le(response + 8, request_id);
    write_u32_le(response + 12, 2);
    write_u16_le(response + 16, 0);

    for (uint32_t index = 0; index < sizeof(response); ++index) {
        serial_write(response[index]);
    }
}

__attribute__((noreturn)) static void protocol_loop(void) {
    uint8_t header[FRAME_HEADER_SIZE];

    for (;;) {
        for (uint32_t index = 0; index < sizeof(header); ++index) {
            header[index] = serial_read();
        }

        uint16_t version = read_u16_le(header + 4);
        uint16_t message_type = read_u16_le(header + 6);
        uint32_t request_id = read_u32_le(header + 8);
        uint32_t payload_length = read_u32_le(header + 12);

        if (payload_length > FRAME_MAX_PAYLOAD) {
            continue;
        }
        if (header[0] != 'C' || header[1] != 'X' || header[2] != 'O' ||
            header[3] != 'S' || version != FRAME_PROTOCOL_VERSION) {
            discard_bytes(payload_length);
            continue;
        }
        if (payload_length != 0) {
            discard_bytes(payload_length);
            continue;
        }
        if (message_type == LIST_TOOLS_REQUEST && request_id != 0) {
            send_empty_tool_list(request_id);
        }
    }
}

__attribute__((noreturn)) void kmain(void) {
    static const char ready[] = "CODEXOS-SEED-READY\n";

    serial_init();
    for (uint64_t index = 0; index < sizeof(ready) - 1; ++index) {
        serial_write((uint8_t)ready[index]);
    }

    protocol_loop();
}
