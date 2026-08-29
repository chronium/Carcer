#ifndef CODEXOS_SEED_BUILD_H
#define CODEXOS_SEED_BUILD_H

#include <stdint.h>

#define FINISH_GENERATION_HANDOFF_MAX (16u * 1024u)

void build_tool_invoke(uint32_t tool_request_id);
void finish_generation_tool_invoke(
    uint32_t tool_request_id,
    const uint8_t *handoff,
    uint32_t handoff_length
);

#endif
