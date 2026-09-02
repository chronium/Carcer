#pragma once
#include <stdint.h>
#define FINISH_GENERATION_HANDOFF_MAX (16u*1024u)
#define FEATURE_REQUEST_TITLE_MAX 256u
#define FEATURE_REQUEST_DESCRIPTION_MAX (16u*1024u)
void build_tool_invoke(uint32_t);void list_provided_assets_tool_invoke(uint32_t);void read_provided_asset_tool_invoke(uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t);int import_provided_asset(const uint8_t*,uint32_t,const uint8_t*,uint32_t);void finish_generation_tool_invoke(uint32_t,const uint8_t*,uint32_t);void request_feature_tool_invoke(uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t);
