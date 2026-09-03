#include <stdint.h>
#include "files.h"
#include "heap.h"
#include "interrupts.h"
#include "memory.h"
#include "protocol.h"
#include "serial.h"
#include "tasks.h"
#include "video.h"
__attribute__((noreturn))static void halt(void){for(;;){__asm__ volatile("cli; hlt");}}__attribute__((noreturn))void kmain(void){static const uint8_t ready[]="CODEXOS-SEED-READY\n";static const uint8_t memory_error[]="CODEXOS-SEED-MEMORY-ERROR\n";static const uint8_t video_error[]="CODEXOS-SEED-VIDEO-ERROR\n";static const uint8_t heap_error[]="CODEXOS-SEED-HEAP-ERROR\n";static const uint8_t store_error[]="CODEXOS-SEED-STORE-ERROR\n";static const uint8_t interrupt_error[]="CODEXOS-SEED-INTERRUPT-ERROR\n";static const uint8_t task_error[]="CODEXOS-SEED-TASK-ERROR\n";si();if(!mi()){swb(memory_error,sizeof(memory_error)-1u);halt();}if(!vinit()){swb(video_error,sizeof(video_error)-1u);halt();}if(!hi()){swb(heap_error,sizeof(heap_error)-1u);halt();}if(!fi()){swb(store_error,sizeof(store_error)-1u);halt();}if(!iinit()){swb(interrupt_error,sizeof(interrupt_error)-1u);halt();}if(!tinit()){swb(task_error,sizeof(task_error)-1u);halt();}swb(ready,sizeof(ready)-1u);ploop();}
