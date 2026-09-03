#pragma once
#include <stdint.h>
struct video_info{uint32_t size,width,height,pitch,format,reserved[3];};
int video_init(void);void video_info(struct video_info*);uint8_t*video_target(uint32_t,uint32_t,uint32_t,uint32_t,uint32_t*);
