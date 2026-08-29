#include "files.h"

static uint8_t contents[FILE_CONTENT_CAPACITY];
static uint32_t content_used;

struct file files[FILE_MAX_COUNT];
uint32_t file_count;

static void copy_bytes(uint8_t *destination, const uint8_t *source, uint32_t length) {
    for (uint32_t index = 0; index < length; ++index) {
        destination[index] = source[index];
    }
}

static void copy_file(struct file *destination, const struct file *source) {
    copy_bytes(destination->path, source->path, source->path_length);
    destination->path_length = source->path_length;
    destination->content_offset = source->content_offset;
    destination->content_length = source->content_length;
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

static int compare_paths(
    const uint8_t *left,
    uint32_t left_length,
    const uint8_t *right,
    uint32_t right_length
) {
    uint32_t shared_length = left_length < right_length ? left_length : right_length;
    for (uint32_t index = 0; index < shared_length; ++index) {
        if (left[index] < right[index]) {
            return -1;
        }
        if (left[index] > right[index]) {
            return 1;
        }
    }
    if (left_length < right_length) {
        return -1;
    }
    return left_length > right_length;
}

static int create_file(
    const uint8_t *path,
    uint32_t path_length,
    const uint8_t *data,
    uint32_t data_length
) {
    if (path_length == 0 || path_length > FILE_MAX_PATH_LENGTH ||
        file_count >= FILE_MAX_COUNT ||
        data_length > FILE_CONTENT_CAPACITY - content_used) {
        return 0;
    }

    uint32_t insertion = 0;
    while (insertion < file_count) {
        int order = compare_paths(
            files[insertion].path,
            files[insertion].path_length,
            path,
            path_length
        );
        if (order == 0) {
            return 0;
        }
        if (order > 0) {
            break;
        }
        ++insertion;
    }

    for (uint32_t index = file_count; index > insertion; --index) {
        copy_file(&files[index], &files[index - 1]);
    }

    struct file *file = &files[insertion];
    copy_bytes(file->path, path, path_length);
    file->path_length = (uint16_t)path_length;
    file->content_offset = content_used;
    file->content_length = data_length;
    copy_bytes(contents + content_used, data, data_length);
    content_used += data_length;
    ++file_count;
    return 1;
}

int files_init(void) {
    file_count = 0;
    content_used = 0;

    if (initial_file_count > FILE_MAX_COUNT) {
        return 0;
    }
    for (uint32_t index = 0; index < initial_file_count; ++index) {
        const struct embedded_file *initial = &initial_files[index];
        uintptr_t size = (uintptr_t)initial->end - (uintptr_t)initial->data;
        if (size > FILE_CONTENT_CAPACITY ||
            !create_file(
                initial->path,
                initial->path_length,
                initial->data,
                (uint32_t)size
            )) {
            return 0;
        }
    }
    return 1;
}

uint32_t file_size(const struct file *file) {
    return file->content_length;
}

const uint8_t *file_content(const struct file *file) {
    return contents + file->content_offset;
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

static int resize_file(struct file *file, uint32_t new_size, int zero_growth) {
    uint32_t old_size = file->content_length;
    uint32_t old_end = file->content_offset + old_size;

    if (new_size == old_size) {
        return 1;
    }
    if (new_size > old_size) {
        uint32_t growth = new_size - old_size;
        if (growth > FILE_CONTENT_CAPACITY - content_used) {
            return 0;
        }
        for (uint32_t index = content_used; index > old_end; --index) {
            contents[index + growth - 1] = contents[index - 1];
        }
        for (uint32_t index = 0; index < file_count; ++index) {
            if (&files[index] != file && files[index].content_offset >= old_end) {
                files[index].content_offset += growth;
            }
        }
        if (zero_growth) {
            for (uint32_t index = old_size; index < new_size; ++index) {
                contents[file->content_offset + index] = 0;
            }
        }
        content_used += growth;
    } else {
        uint32_t shrinkage = old_size - new_size;
        for (uint32_t index = old_end; index < content_used; ++index) {
            contents[index - shrinkage] = contents[index];
        }
        for (uint32_t index = 0; index < file_count; ++index) {
            if (&files[index] != file && files[index].content_offset >= old_end) {
                files[index].content_offset -= shrinkage;
            }
        }
        content_used -= shrinkage;
    }
    file->content_length = new_size;
    return 1;
}

int file_write(
    const uint8_t *path,
    uint32_t path_length,
    uint32_t offset,
    const uint8_t *data,
    uint32_t data_length
) {
    struct file *file = file_find(path, path_length);
    if (file == (struct file *)0) {
        return offset == 0 && create_file(path, path_length, data, data_length);
    }
    if (offset > file->content_length ||
        data_length > FILE_CONTENT_CAPACITY - offset) {
        return 0;
    }

    uint32_t new_size = offset + data_length;
    if (new_size > file->content_length && !resize_file(file, new_size, 0)) {
        return 0;
    }
    copy_bytes(contents + file->content_offset + offset, data, data_length);
    return 1;
}

int file_truncate(const uint8_t *path, uint32_t path_length, uint32_t size) {
    struct file *file = file_find(path, path_length);
    return file != (struct file *)0 && resize_file(file, size, 1);
}

int file_remove(const uint8_t *path, uint32_t path_length) {
    struct file *file = file_find(path, path_length);
    if (file == (struct file *)0) {
        return 0;
    }

    uint32_t removal = (uint32_t)(file - files);
    uint32_t removed_size = file->content_length;
    uint32_t removed_end = file->content_offset + removed_size;
    if (removed_size != 0) {
        for (uint32_t index = removed_end; index < content_used; ++index) {
            contents[index - removed_size] = contents[index];
        }
        for (uint32_t index = 0; index < file_count; ++index) {
            if (&files[index] != file &&
                files[index].content_offset >= removed_end) {
                files[index].content_offset -= removed_size;
            }
        }
        content_used -= removed_size;
    }

    for (uint32_t index = removal; index + 1 < file_count; ++index) {
        copy_file(&files[index], &files[index + 1]);
    }
    --file_count;
    return 1;
}
