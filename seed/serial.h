#ifndef CODEXOS_SEED_SERIAL_H
#define CODEXOS_SEED_SERIAL_H

#include <stdint.h>

void serial_init(void);
uint8_t serial_read(void);
void serial_write(uint8_t byte);
void serial_write_bytes(const uint8_t *data, uint32_t length);

#endif
