#include "files.h"

extern uint8_t _binary_seed_files_c_start[];
extern uint8_t _binary_seed_files_c_end[];
extern uint8_t _binary_seed_files_h_start[];
extern uint8_t _binary_seed_files_h_end[];
extern uint8_t _binary_seed_kernel_c_start[];
extern uint8_t _binary_seed_kernel_c_end[];
extern uint8_t _binary_seed_limine_conf_start[];
extern uint8_t _binary_seed_limine_conf_end[];
extern uint8_t _binary_seed_linker_ld_start[];
extern uint8_t _binary_seed_linker_ld_end[];
extern uint8_t _binary_seed_protocol_c_start[];
extern uint8_t _binary_seed_protocol_c_end[];
extern uint8_t _binary_seed_protocol_h_start[];
extern uint8_t _binary_seed_protocol_h_end[];
extern uint8_t _binary_seed_serial_c_start[];
extern uint8_t _binary_seed_serial_c_end[];
extern uint8_t _binary_seed_serial_h_start[];
extern uint8_t _binary_seed_serial_h_end[];
extern uint8_t _binary_seed_tools_c_start[];
extern uint8_t _binary_seed_tools_c_end[];
extern uint8_t _binary_seed_tools_h_start[];
extern uint8_t _binary_seed_tools_h_end[];

struct file files[FILE_COUNT] = {
    {
        (const uint8_t *)"seed/files.c",
        sizeof("seed/files.c") - 1,
        _binary_seed_files_c_start,
        _binary_seed_files_c_end,
    },
    {
        (const uint8_t *)"seed/files.h",
        sizeof("seed/files.h") - 1,
        _binary_seed_files_h_start,
        _binary_seed_files_h_end,
    },
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
    {
        (const uint8_t *)"seed/protocol.c",
        sizeof("seed/protocol.c") - 1,
        _binary_seed_protocol_c_start,
        _binary_seed_protocol_c_end,
    },
    {
        (const uint8_t *)"seed/protocol.h",
        sizeof("seed/protocol.h") - 1,
        _binary_seed_protocol_h_start,
        _binary_seed_protocol_h_end,
    },
    {
        (const uint8_t *)"seed/serial.c",
        sizeof("seed/serial.c") - 1,
        _binary_seed_serial_c_start,
        _binary_seed_serial_c_end,
    },
    {
        (const uint8_t *)"seed/serial.h",
        sizeof("seed/serial.h") - 1,
        _binary_seed_serial_h_start,
        _binary_seed_serial_h_end,
    },
    {
        (const uint8_t *)"seed/tools.c",
        sizeof("seed/tools.c") - 1,
        _binary_seed_tools_c_start,
        _binary_seed_tools_c_end,
    },
    {
        (const uint8_t *)"seed/tools.h",
        sizeof("seed/tools.h") - 1,
        _binary_seed_tools_h_start,
        _binary_seed_tools_h_end,
    },
};

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

uint32_t file_size(const struct file *file) {
    return (uint32_t)((uintptr_t)file->end - (uintptr_t)file->data);
}

struct file *file_find(const uint8_t *path, uint32_t path_length) {
    for (uint32_t index = 0; index < FILE_COUNT; ++index) {
        if (bytes_equal(
                path,
                path_length,
                files[index].path,
                files[index].path_length
            )) {
            return &files[index];
        }
    }
    return (struct file *)0;
}

int file_path_has_prefix(
    const struct file *file,
    const uint8_t *prefix,
    uint32_t prefix_length
) {
    if (prefix_length > file->path_length) {
        return 0;
    }
    return bytes_equal(file->path, prefix_length, prefix, prefix_length);
}
