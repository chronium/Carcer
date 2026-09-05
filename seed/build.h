#pragma once
#include <stdint.h>
#define HM (16u*1024u)
#define QTM 256u
#define QDM (16u*1024u)
void tbuild(uint32_t);void tassets(uint32_t);void tassetread(uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t);int aimp(const uint8_t*,uint32_t,const uint8_t*,uint32_t);void tfinish(uint32_t,const uint8_t*,uint32_t);void trequest(uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t);

/* Sequential development host-service correlation IDs; task0 owns the stream. */
uint32_t hostid(void);
