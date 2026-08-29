#include <stdint.h>

#define COM1 0x3f8u
#define FRAME_HEADER_SIZE 16u
#define FRAME_MAX_PAYLOAD (16u * 1024u * 1024u)
#define FRAME_PROTOCOL_VERSION 1u
#define LIST_TOOLS_REQUEST 0x0001u
#define LIST_TOOLS_RESPONSE 0x8001u
#define INVOKE_TOOL_REQUEST 0x0002u
#define INVOKE_TOOL_RESPONSE 0x8002u
#define REQUEST_BUFFER_SIZE 1024u
#define MAX_TOOL_ARGUMENTS 3u
#define FILE_COUNT 3u
#define TOOL_SUCCESS 0u
#define TOOL_FAILURE 1u

extern uint8_t _binary_seed_kernel_c_start[];
extern uint8_t _binary_seed_kernel_c_end[];
extern uint8_t _binary_seed_limine_conf_start[];
extern uint8_t _binary_seed_limine_conf_end[];
extern uint8_t _binary_seed_linker_ld_start[];
extern uint8_t _binary_seed_linker_ld_end[];

struct bytes {
    const uint8_t *data;
    uint32_t length;
};

struct invocation {
    struct bytes name;
    uint16_t argument_count;
    struct bytes arguments[MAX_TOOL_ARGUMENTS];
};

struct file {
    const uint8_t *path;
    uint32_t path_length;
    uint8_t *data;
    uint8_t *end;
};

static struct file files[FILE_COUNT] = {
    {
        (const uint8_t *)"seed/kernel.c",
        sizeof("seed/kernel.c") - 1,
        _binary_seed_kernel_c_start,
        _binary_seed_kernel_c_end,
    },
    {
        (const uint8_t *)"seed/limine.conf",
        sizeof("seed/limine.conf") - 1,
        _binary_seed_limine_conf_start,
        _binary_seed_limine_conf_end,
    },
    {
        (const uint8_t *)"seed/linker.ld",
        sizeof("seed/linker.ld") - 1,
        _binary_seed_linker_ld_start,
        _binary_seed_linker_ld_end,
    },
};

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

static void serial_write_bytes(const uint8_t *data, uint32_t length) {
    for (uint32_t index = 0; index < length; ++index) {
        serial_write(data[index]);
    }
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

static int bytes_equal(
    const uint8_t *left,
    uint32_t left_length,
    const uint8_t *right,
    uint32_t right_length
) {
    if (left_length != right_length) {
        return 0;
    }
    for (uint32_t index = 0; index < left_length; ++index) {
        if (left[index] != right[index]) {
            return 0;
        }
    }
    return 1;
}

static int valid_utf8(const uint8_t *data, uint32_t length) {
    uint32_t index = 0;

    while (index < length) {
        uint8_t first = data[index++];
        uint32_t codepoint;
        uint32_t minimum;
        uint32_t remaining;

        if (first <= 0x7f) {
            continue;
        }
        if (first >= 0xc2 && first <= 0xdf) {
            codepoint = first & 0x1f;
            minimum = 0x80;
            remaining = 1;
        } else if (first >= 0xe0 && first <= 0xef) {
            codepoint = first & 0x0f;
            minimum = 0x800;
            remaining = 2;
        } else if (first >= 0xf0 && first <= 0xf4) {
            codepoint = first & 0x07;
            minimum = 0x10000;
            remaining = 3;
        } else {
            return 0;
        }

        if (remaining > length - index) {
            return 0;
        }
        for (uint32_t count = 0; count < remaining; ++count) {
            uint8_t next = data[index++];
            if ((next & 0xc0) != 0x80) {
                return 0;
            }
            codepoint = (codepoint << 6) | (next & 0x3f);
        }
        if (codepoint < minimum || codepoint > 0x10ffff ||
            (codepoint >= 0xd800 && codepoint <= 0xdfff)) {
            return 0;
        }
    }
    return 1;
}

static int parse_decimal(struct bytes argument, uint32_t *value) {
    uint32_t parsed = 0;

    if (argument.length == 0) {
        return 0;
    }
    for (uint32_t index = 0; index < argument.length; ++index) {
        uint8_t byte = argument.data[index];
        if (byte < '0' || byte > '9') {
            return 0;
        }
        uint32_t digit = byte - '0';
        if (parsed > (0xffffffffu - digit) / 10u) {
            return 0;
        }
        parsed = parsed * 10u + digit;
    }
    *value = parsed;
    return 1;
}

static uint32_t file_size(const struct file *file) {
    return (uint32_t)((uintptr_t)file->end - (uintptr_t)file->data);
}

static struct file *find_file(struct bytes path) {
    for (uint32_t index = 0; index < FILE_COUNT; ++index) {
        if (bytes_equal(
                path.data,
                path.length,
                files[index].path,
                files[index].path_length
            )) {
            return &files[index];
        }
    }
    return (struct file *)0;
}

static int path_has_prefix(const struct file *file, struct bytes prefix) {
    if (prefix.length > file->path_length) {
        return 0;
    }
    return bytes_equal(file->path, prefix.length, prefix.data, prefix.length);
}

static void send_frame_header(
    uint16_t message_type,
    uint32_t request_id,
    uint32_t payload_length
) {
    uint8_t header[FRAME_HEADER_SIZE];

    header[0] = 'C';
    header[1] = 'X';
    header[2] = 'O';
    header[3] = 'S';
    write_u16_le(header + 4, FRAME_PROTOCOL_VERSION);
    write_u16_le(header + 6, message_type);
    write_u32_le(header + 8, request_id);
    write_u32_le(header + 12, payload_length);
    serial_write_bytes(header, sizeof(header));
}

static void serial_write_u16_le(uint16_t value) {
    uint8_t bytes[2];
    write_u16_le(bytes, value);
    serial_write_bytes(bytes, sizeof(bytes));
}

static void serial_write_u32_le(uint32_t value) {
    uint8_t bytes[4];
    write_u32_le(bytes, value);
    serial_write_bytes(bytes, sizeof(bytes));
}

static void send_tool_list(uint32_t request_id) {
    static const uint8_t list_name[] = "list";
    static const uint8_t read_name[] = "read";
    uint32_t payload_length = 2u + 2u + (sizeof(list_name) - 1u) + 2u +
                              (sizeof(read_name) - 1u);

    send_frame_header(LIST_TOOLS_RESPONSE, request_id, payload_length);
    serial_write_u16_le(2);
    serial_write_u16_le(sizeof(list_name) - 1u);
    serial_write_bytes(list_name, sizeof(list_name) - 1u);
    serial_write_u16_le(sizeof(read_name) - 1u);
    serial_write_bytes(read_name, sizeof(read_name) - 1u);
}

static void send_invoke_header(
    uint32_t request_id,
    uint32_t status,
    uint32_t output_length
) {
    send_frame_header(INVOKE_TOOL_RESPONSE, request_id, 4u + output_length);
    serial_write_u32_le(status);
}

static void send_tool_failure(uint32_t request_id) {
    send_invoke_header(request_id, TOOL_FAILURE, 0);
}

static int parse_invocation(
    const uint8_t *payload,
    uint32_t payload_length,
    struct invocation *invocation
) {
    uint32_t offset = 0;

    if (payload_length < 2) {
        return 0;
    }
    uint16_t name_length = read_u16_le(payload);
    offset += 2;
    if (name_length == 0 || name_length > 255 ||
        name_length > payload_length - offset) {
        return 0;
    }
    invocation->name.data = payload + offset;
    invocation->name.length = name_length;
    if (!valid_utf8(invocation->name.data, invocation->name.length)) {
        return 0;
    }
    offset += name_length;

    if (payload_length - offset < 2) {
        return 0;
    }
    invocation->argument_count = read_u16_le(payload + offset);
    offset += 2;
    if (invocation->argument_count > MAX_TOOL_ARGUMENTS) {
        return 0;
    }

    for (uint16_t index = 0; index < invocation->argument_count; ++index) {
        if (payload_length - offset < 4) {
            return 0;
        }
        uint32_t argument_length = read_u32_le(payload + offset);
        offset += 4;
        if (argument_length > payload_length - offset) {
            return 0;
        }
        invocation->arguments[index].data = payload + offset;
        invocation->arguments[index].length = argument_length;
        offset += argument_length;
    }
    return offset == payload_length;
}

static void invoke_list(uint32_t request_id, const struct invocation *invocation) {
    struct bytes prefix = {(const uint8_t *)0, 0};

    if (invocation->argument_count > 1) {
        send_tool_failure(request_id);
        return;
    }
    if (invocation->argument_count == 1) {
        prefix = invocation->arguments[0];
        if (!valid_utf8(prefix.data, prefix.length)) {
            send_tool_failure(request_id);
            return;
        }
    }

    uint32_t output_length = 0;
    for (uint32_t index = 0; index < FILE_COUNT; ++index) {
        if (path_has_prefix(&files[index], prefix)) {
            output_length += files[index].path_length + 1u;
        }
    }

    send_invoke_header(request_id, TOOL_SUCCESS, output_length);
    for (uint32_t index = 0; index < FILE_COUNT; ++index) {
        if (path_has_prefix(&files[index], prefix)) {
            serial_write_bytes(files[index].path, files[index].path_length);
            serial_write('\n');
        }
    }
}

static void invoke_read(uint32_t request_id, const struct invocation *invocation) {
    uint32_t offset;
    uint32_t requested_length;

    if (invocation->argument_count != 3 ||
        !valid_utf8(
            invocation->arguments[0].data,
            invocation->arguments[0].length
        ) ||
        !parse_decimal(invocation->arguments[1], &offset) ||
        !parse_decimal(invocation->arguments[2], &requested_length) ||
        requested_length > FRAME_MAX_PAYLOAD - 4u) {
        send_tool_failure(request_id);
        return;
    }

    struct file *file = find_file(invocation->arguments[0]);
    if (file == (struct file *)0) {
        send_tool_failure(request_id);
        return;
    }

    uint32_t size = file_size(file);
    if (offset > size) {
        send_tool_failure(request_id);
        return;
    }
    uint32_t available = size - offset;
    uint32_t output_length = requested_length < available ? requested_length : available;

    send_invoke_header(request_id, TOOL_SUCCESS, output_length);
    serial_write_bytes(file->data + offset, output_length);
}

static void handle_invocation(
    uint32_t request_id,
    const uint8_t *payload,
    uint32_t payload_length
) {
    static const uint8_t list_name[] = "list";
    static const uint8_t read_name[] = "read";
    struct invocation invocation;

    if (!parse_invocation(payload, payload_length, &invocation)) {
        send_tool_failure(request_id);
        return;
    }
    if (bytes_equal(
            invocation.name.data,
            invocation.name.length,
            list_name,
            sizeof(list_name) - 1u
        )) {
        invoke_list(request_id, &invocation);
    } else if (bytes_equal(
                   invocation.name.data,
                   invocation.name.length,
                   read_name,
                   sizeof(read_name) - 1u
               )) {
        invoke_read(request_id, &invocation);
    } else {
        send_tool_failure(request_id);
    }
}

__attribute__((noreturn)) static void protocol_loop(void) {
    uint8_t header[FRAME_HEADER_SIZE];
    uint8_t payload[REQUEST_BUFFER_SIZE];

    for (;;) {
        for (uint32_t index = 0; index < sizeof(header); ++index) {
            header[index] = serial_read();
        }

        uint16_t version = read_u16_le(header + 4);
        uint16_t message_type = read_u16_le(header + 6);
        uint32_t request_id = read_u32_le(header + 8);
        uint32_t payload_length = read_u32_le(header + 12);

        if (payload_length > FRAME_MAX_PAYLOAD) {
            for (;;) {
                __asm__ volatile("pause");
            }
        }

        int valid_header = header[0] == 'C' && header[1] == 'X' &&
                           header[2] == 'O' && header[3] == 'S' &&
                           version == FRAME_PROTOCOL_VERSION;
        if (payload_length > sizeof(payload)) {
            discard_bytes(payload_length);
            if (valid_header && message_type == INVOKE_TOOL_REQUEST &&
                request_id != 0) {
                send_tool_failure(request_id);
            }
            continue;
        }
        for (uint32_t index = 0; index < payload_length; ++index) {
            payload[index] = serial_read();
        }

        if (!valid_header || request_id == 0) {
            continue;
        }
        if (message_type == LIST_TOOLS_REQUEST && payload_length == 0) {
            send_tool_list(request_id);
        } else if (message_type == INVOKE_TOOL_REQUEST) {
            handle_invocation(request_id, payload, payload_length);
        }
    }
}

__attribute__((noreturn)) void kmain(void) {
    static const uint8_t ready[] = "CODEXOS-SEED-READY\n";

    serial_init();
    serial_write_bytes(ready, sizeof(ready) - 1u);
    protocol_loop();
}
