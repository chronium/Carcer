#ifndef CODEXOS_SEED_INTERRUPTS_H
#define CODEXOS_SEED_INTERRUPTS_H
#include <stdint.h>
#define TIMER_FREQUENCY_HZ 100u
int interrupts_init(void);
uint64_t timer_ticks(void);
#endif
