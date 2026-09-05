#pragma once
#include <stdint.h>
void tbootstrap(uint32_t,const uint8_t*,uint32_t);
void tbootread(uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t);
void tbootimport(uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t,const uint8_t*,uint32_t);
int boottest(void);
int bootlive(void);
