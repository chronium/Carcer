#pragma once
#include <stdint.h>
#define SS_CONTENT_MAX 1048576u
#define SS_SERIALIZED_MAX 1081986u
int ssmeasure(uint16_t *, uint32_t *);
void sswrite(uint16_t);
int sstest(void);
