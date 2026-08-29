#ifndef CODEXOS_SEED_FILES_H
#define CODEXOS_SEED_FILES_H

#include <stdint.h>

struct file {
    const uint8_t *path;
    uint32_t path_length;
    uint8_t *data;
    uint8_t *end;
};

extern struct file files[];
extern const uint32_t file_count;

uint32_t file_size(const struct file *file);
struct file *file_find(const uint8_t *path, uint32_t path_length);
int file_path_has_prefix(
    const struct file *file,
    const uint8_t *prefix,
    uint32_t prefix_length
);

#endif
