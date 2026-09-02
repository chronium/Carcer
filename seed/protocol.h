#pragma once
#include <stdint.h>
#define LIST_TOOLS_REQUEST 1u
#define LIST_TOOLS_RESPONSE 0x8001u
#define INVOKE_TOOL_REQUEST 2u
#define INVOKE_TOOL_RESPONSE 0x8002u
#define HOST_SERVICE_REQUEST 3u
#define HOST_SERVICE_RESPONSE 0x8003u
#define FRAME_MAX_PAYLOAD (128u*1024u*1024u)
void frame_send_header(uint16_t,uint32_t,uint32_t);void frame_write_u16(uint16_t);void frame_write_u32(uint32_t);__attribute__((noreturn))void protocol_loop(void);
