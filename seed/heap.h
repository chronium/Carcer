#ifndef CODEXOS_SEED_HEAP_H
#define CODEXOS_SEED_HEAP_H
#include <stdint.h>
struct heap_stats {
 uint64_t reserved_pages;
 uint64_t active_allocations;
 uint64_t requested_bytes;
 uint64_t free_bytes;
};
int heap_init(void);
void *heap_alloc(uint64_t size);
void *heap_calloc(uint64_t count, uint64_t size);
int heap_free(void *pointer);
struct heap_stats heap_get_stats(void);
#endif
