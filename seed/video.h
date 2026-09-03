#pragma once
#include <stdint.h>
struct vinfo{uint32_t size,width,height,pitch,format,reserved[3];};
int vinit(void);void vinfo(struct vinfo*);uint8_t*vtarget(uint32_t,uint32_t,uint32_t,uint32_t,uint32_t*);
