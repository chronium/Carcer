#pragma once
#include <stdint.h>
#define KEY_BATCH 64u
struct key_event {
    uint64_t sequence,tick;
    uint16_t code;
    uint8_t pressed,flags;
    uint32_t reserved;
};
int key_init(void);
void key_poll(void);
int key_available(void);
int key_read(uint64_t,struct key_event *,uint32_t,uint64_t *);
int key_tests(void);
int key_fixture(int);
