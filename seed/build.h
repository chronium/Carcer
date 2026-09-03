#pragma once
#include <stdint.h>
#define FINISH_GENERATION_HANDOFF_MAX (16u*1024u)
#define FEATURE_REQUEST_TITLE_MAX 256u
#define FEATURE_REQUEST_DESCRIPTION_MAX (16u*1024u)
void tool_build(uint32_t);void tool_assets(uint32_t);void tool_asset_read(uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t);int asset_import(const uint8_t*,uint32_t,const uint8_t*,uint32_t);void tool_finish(uint32_t,const uint8_t*,uint32_t);void tool_request(uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t);
