#pragma once
#include <stdint.h>
int heap_init(void);void*heap_alloc(uint64_t);int heap_free(void*);
