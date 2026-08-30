#ifndef CODEXOS_SEED_PROTOCOL_H
#define CODEXOS_SEED_PROTOCOL_H
#include <stdint.h>
#define FRAME_MAX_PAYLOAD (16u * 1024u * 1024u)
#define LIST_TOOLS_REQUEST 0x0001u
#define LIST_TOOLS_RESPONSE 0x8001u
#define INVOKE_TOOL_REQUEST 0x0002u
#define INVOKE_TOOL_RESPONSE 0x8002u
#define HOST_SERVICE_REQUEST 0x0003u
#define HOST_SERVICE_RESPONSE 0x8003u
void frame_send_header(
 uint16_t message_type,
 uint32_t request_id,
 uint32_t payload_length
);
void frame_write_u16(uint16_t value);
void frame_write_u32(uint32_t value);
__attribute__((noreturn)) void protocol_loop(void);
#endif
