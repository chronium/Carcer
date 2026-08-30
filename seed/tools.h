#ifndef CODEXOS_SEED_TOOLS_H
#define CODEXOS_SEED_TOOLS_H
#include <stdint.h>
void tools_send_list(uint32_t request_id);
void tools_send_failure(uint32_t request_id);
void tools_handle_invocation(
 uint32_t request_id,
 const uint8_t *payload,
 uint32_t payload_length
);
#endif
