#ifndef CODEXOS_SEED_MEMORY_H
#define CODEXOS_SEED_MEMORY_H

#include <stdint.h>

#define MEMORY_PAGE_SIZE 4096u

int memory_init(void);
uint64_t memory_alloc_page(void);
int memory_free_page(uint64_t physical_address);
void *memory_physical_to_virtual(uint64_t physical_address);
uint64_t memory_total_pages(void);
uint64_t memory_free_pages(void);

#endif
