#include "files.h"

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
    for (uint32_t index = 0; index < file_count; ++index) {
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
