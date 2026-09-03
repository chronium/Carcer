#pragma once
#include <stdint.h>
#define TIMER_FREQUENCY_HZ 100u
int interrupts_init(void);void interrupts_set_task_stack(void*);uint64_t timer_ticks(void);
