#include <stdint.h>

#include "protocol.h"
#include "serial.h"

__attribute__((noreturn)) void kmain(void) {
    static const uint8_t ready[] = "CODEXOS-SEED-READY\n";

    serial_init();
    serial_write_bytes(ready, sizeof(ready) - 1u);
    protocol_loop();
}
