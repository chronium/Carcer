#pragma once
#include <stdint.h>
#define FMC 128u
#define FPL 255u
#define FIM 1u
struct file{uint8_t p[FPL];uint16_t n;uint8_t*d;uint32_t z,c,a;};struct embedded_file{const uint8_t*p;uint16_t n;const uint8_t*data,*end;};extern const struct embedded_file initial_files[];extern const uint32_t initial_file_count;extern struct file files[FMC];extern uint32_t fc;int fu(const uint8_t*,uint32_t);int fpv(const uint8_t*,uint32_t);int fi(void);uint32_t fz(const struct file*);const uint8_t*fd(const struct file*);uint32_t fa(const struct file*);struct file*ff(const uint8_t*,uint32_t);int fpp(const struct file*,const uint8_t*,uint32_t);int fws(const uint8_t*,uint32_t,uint32_t,uint32_t,uint8_t**);int fw(const uint8_t*,uint32_t,uint32_t,const uint8_t*,uint32_t);int ft(const uint8_t*,uint32_t,uint32_t);int fr(const uint8_t*,uint32_t);int fsl(const uint8_t*,uint32_t);
