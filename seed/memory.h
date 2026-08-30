#ifndef CODEXOS_SEED_MEMORY_H
#define CODEXOS_SEED_MEMORY_H
#include <stdint.h>
#define MEMORY_PAGE_SIZE 4096u
int memory_init(void);uint64_t memory_page_alloc(void);uint64_t memory_pages_alloc(uint64_t page_count);int memory_page_free(uint64_t physical_address);int memory_pages_free(uint64_t physical_address,uint64_t page_count);void*memory_physical_to_virtual(uint64_t physical_address);
#endif
