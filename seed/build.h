#ifndef CODEXOS_SEED_BUILD_H
#define CODEXOS_SEED_BUILD_H
#include <stdint.h>
#define FINISH_GENERATION_HANDOFF_MAX (16u * 1024u)
#define FEATURE_REQUEST_TITLE_MAX 256u
#define FEATURE_REQUEST_DESCRIPTION_MAX (16u * 1024u)
void build_tool_invoke(uint32_t tool_request_id);
void finish_generation_tool_invoke(
uint32_t tool_request_id,
const uint8_t *handoff,
uint32_t handoff_length
);
void request_feature_tool_invoke(
uint32_t tool_request_id,
const uint8_t *title,
uint32_t title_length,
const uint8_t *description,
uint32_t description_length
);
#endif
