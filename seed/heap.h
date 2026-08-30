#ifndef CODEXOS_SEED_HEAP_H
#define CODEXOS_SEED_HEAP_H
#include <stdint.h>
int heap_init(void);void*heap_alloc(uint64_t size);int heap_free(void*pointer);
#endif
