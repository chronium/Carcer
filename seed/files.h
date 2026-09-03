#pragma once
#include <stdint.h>
#define FILE_MAX_COUNT 128u
#define FILE_MAX_PATH_LENGTH 255u
#define FILE_ATTRIBUTE_IMMUTABLE 1u
struct file{uint8_t path[FILE_MAX_PATH_LENGTH];uint16_t path_length;uint8_t*content;uint32_t content_length,content_capacity,attributes;};struct embedded_file{const uint8_t*path;uint16_t path_length;const uint8_t*data,*end;};extern const struct embedded_file initial_files[];extern const uint32_t initial_file_count;extern struct file files[FILE_MAX_COUNT];extern uint32_t file_count;int file_utf8(const uint8_t*,uint32_t);int file_path_valid(const uint8_t*,uint32_t);int files_init(void);uint32_t file_size(const struct file*);const uint8_t*file_content(const struct file*);uint32_t file_attributes(const struct file*);struct file*file_find(const uint8_t*,uint32_t);int file_path_has_prefix(const struct file*,const uint8_t*,uint32_t);int file_write_span(const uint8_t*,uint32_t,uint32_t,uint32_t,uint8_t**);int file_write(const uint8_t*,uint32_t,uint32_t,const uint8_t*,uint32_t);int file_truncate(const uint8_t*,uint32_t,uint32_t);int file_remove(const uint8_t*,uint32_t);int file_seal(const uint8_t*,uint32_t);
