#pragma once
#include <stdint.h>
#define LIST_TOOLS_REQUEST 1u
#define LIST_TOOLS_RESPONSE 0x8001u
#define INVOKE_TOOL_REQUEST 2u
#define INVOKE_TOOL_RESPONSE 0x8002u
#define HOST_SERVICE_REQUEST 3u
#define HOST_SERVICE_RESPONSE 0x8003u
#define FM (128u*1024u*1024u)
void ph(uint16_t,uint32_t,uint32_t);void pw16(uint16_t);void pw32(uint32_t);__attribute__((noreturn))void ploop(void);
