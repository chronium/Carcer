#pragma once
#include <stdint.h>
void serial_init(void);uint8_t serial_read(void);void serial_write(uint8_t);void serial_write_bytes(const uint8_t*,uint32_t);
