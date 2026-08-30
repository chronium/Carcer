#ifndef CODEXOS_SEED_FILES_H
#define CODEXOS_SEED_FILES_H

#include <stdint.h>

#define FILE_MAX_COUNT 128u
#define FILE_MAX_PATH_LENGTH 255u

struct file {
    uint8_t path[FILE_MAX_PATH_LENGTH];
    uint16_t path_length;
    uint8_t *content;
    uint32_t content_length;
    uint32_t content_capacity;
};

struct embedded_file {
    const uint8_t *path;
    uint16_t path_length;
    const uint8_t *data;
    const uint8_t *end;
};

extern const struct embedded_file initial_files[];
extern const uint32_t initial_file_count;
extern struct file files[FILE_MAX_COUNT];
extern uint32_t file_count;

int files_init(void);
uint32_t file_size(const struct file *file);
const uint8_t *file_content(const struct file *file);
struct file *file_find(const uint8_t *path, uint32_t path_length);
int file_path_has_prefix(
    const struct file *file,
    const uint8_t *prefix,
    uint32_t prefix_length
);
int file_write(
    const uint8_t *path,
    uint32_t path_length,
    uint32_t offset,
    const uint8_t *data,
    uint32_t data_length
);
int file_truncate(const uint8_t *path, uint32_t path_length, uint32_t size);
int file_remove(const uint8_t *path, uint32_t path_length);

#endif
