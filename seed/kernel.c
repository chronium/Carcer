#include <stdint.h>

#include "files.h"
#include "protocol.h"
#include "serial.h"

__attribute__((noreturn)) void kmain(void) {
    static const uint8_t ready[] = "CODEXOS-SEED-READY\n";
    static const uint8_t store_error[] = "CODEXOS-SEED-STORE-ERROR\n";

    serial_init();
    if (!files_init()) {
        serial_write_bytes(store_error, sizeof(store_error) - 1u);
        for (;;) {
            __asm__ volatile("pause");
        }
    }
    serial_write_bytes(ready, sizeof(ready) - 1u);
    protocol_loop();
}
