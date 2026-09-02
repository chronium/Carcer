#pragma once
#include <stdint.h>
#define MEMORY_PAGE_SIZE 4096u
int memory_init(void);uint64_t memory_free_page_count(void);uint64_t memory_page_alloc(void);uint64_t memory_pages_alloc(uint64_t);int memory_page_free(uint64_t);int memory_pages_free(uint64_t,uint64_t);void*memory_physical_to_virtual(uint64_t);
