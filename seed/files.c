#include "files.h"

#include "heap.h"

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
    destination->content = source->content;
    destination->content_length = source->content_length;
    destination->content_capacity = source->content_capacity;
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

static int reserve_content(struct file *file, uint32_t required_capacity) {
    if (required_capacity <= file->content_capacity) {
        return 1;
    }

    uint32_t capacity = file->content_capacity;
    if (capacity < 256u) {
        capacity = 256u;
    }
    while (capacity < required_capacity) {
        if (capacity > UINT32_MAX / 2u) {
            capacity = required_capacity;
            break;
        }
        capacity *= 2u;
    }

    uint8_t *content = (uint8_t *)heap_alloc(capacity);
    if (content == (uint8_t *)0) {
        return 0;
    }
    copy_bytes(content, file->content, file->content_length);
    if (file->content != (uint8_t *)0 && !heap_free(file->content)) {
        (void)heap_free(content);
        return 0;
    }
    file->content = content;
    file->content_capacity = capacity;
    return 1;
}

static int create_file(
    const uint8_t *path,
    uint32_t path_length,
    const uint8_t *data,
    uint32_t data_length
) {
    if (path_length == 0 || path_length > FILE_MAX_PATH_LENGTH ||
        file_count >= FILE_MAX_COUNT) {
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

    struct file incoming;
    incoming.path_length = (uint16_t)path_length;
    incoming.content = (uint8_t *)0;
    incoming.content_length = 0;
    incoming.content_capacity = 0;
    copy_bytes(incoming.path, path, path_length);
    if (data_length != 0) {
        if (!reserve_content(&incoming, data_length)) {
            return 0;
        }
        copy_bytes(incoming.content, data, data_length);
        incoming.content_length = data_length;
    }

    for (uint32_t index = file_count; index > insertion; --index) {
        copy_file(&files[index], &files[index - 1]);
    }
    copy_file(&files[insertion], &incoming);
    ++file_count;
    return 1;
}

static void discard_all_files(void) {
    while (file_count != 0) {
        --file_count;
        if (files[file_count].content != (uint8_t *)0) {
            (void)heap_free(files[file_count].content);
            files[file_count].content = (uint8_t *)0;
        }
    }
}

int files_init(void) {
    file_count = 0;

    if (initial_file_count > FILE_MAX_COUNT) {
        return 0;
    }
    for (uint32_t index = 0; index < initial_file_count; ++index) {
        const struct embedded_file *initial = &initial_files[index];
        uintptr_t size = (uintptr_t)initial->end - (uintptr_t)initial->data;
        if (size > UINT32_MAX ||
            !create_file(
                initial->path,
                initial->path_length,
                initial->data,
                (uint32_t)size
            )) {
            discard_all_files();
            return 0;
        }
    }
    return 1;
}

uint32_t file_size(const struct file *file) {
    return file->content_length;
}

const uint8_t *file_content(const struct file *file) {
    return file->content;
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
    if (new_size == old_size) {
        return 1;
    }

    if (new_size == 0) {
        if (file->content != (uint8_t *)0 && !heap_free(file->content)) {
            return 0;
        }
        file->content = (uint8_t *)0;
        file->content_length = 0;
        file->content_capacity = 0;
        return 1;
    }

    if (!reserve_content(file, new_size)) {
        return 0;
    }
    if (zero_growth && new_size > old_size) {
        for (uint32_t index = old_size; index < new_size; ++index) {
            file->content[index] = 0;
        }
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
        data_length > UINT32_MAX - offset) {
        return 0;
    }
    if (data_length == 0) {
        return 1;
    }

    uint32_t new_size = offset + data_length;
    if (new_size > file->content_length && !resize_file(file, new_size, 0)) {
        return 0;
    }
    copy_bytes(file->content + offset, data, data_length);
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
    if (file->content != (uint8_t *)0 && !heap_free(file->content)) {
        return 0;
    }
    for (uint32_t index = removal; index + 1 < file_count; ++index) {
        copy_file(&files[index], &files[index + 1]);
    }
    --file_count;
    return 1;
}
