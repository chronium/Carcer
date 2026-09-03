#include <stdint.h>
#include "files.h"
#include "heap.h"
#include "interrupts.h"
#include "memory.h"
#include "protocol.h"
#include "serial.h"
#include "tasks.h"
#include "video.h"
__attribute__((noreturn))static void halt(void){for(;;){__asm__ volatile("cli; hlt");}}__attribute__((noreturn))void kmain(void){static const uint8_t ready[]="CODEXOS-SEED-READY\n";static const uint8_t memory_error[]="CODEXOS-SEED-MEMORY-ERROR\n";static const uint8_t video_error[]="CODEXOS-SEED-VIDEO-ERROR\n";static const uint8_t heap_error[]="CODEXOS-SEED-HEAP-ERROR\n";static const uint8_t store_error[]="CODEXOS-SEED-STORE-ERROR\n";static const uint8_t interrupt_error[]="CODEXOS-SEED-INTERRUPT-ERROR\n";static const uint8_t task_error[]="CODEXOS-SEED-TASK-ERROR\n";serial_init();if(!memory_init()){serial_write_bytes(memory_error,sizeof(memory_error)-1u);halt();}if(!video_init()){serial_write_bytes(video_error,sizeof(video_error)-1u);halt();}if(!heap_init()){serial_write_bytes(heap_error,sizeof(heap_error)-1u);halt();}if(!files_init()){serial_write_bytes(store_error,sizeof(store_error)-1u);halt();}if(!interrupts_init()){serial_write_bytes(interrupt_error,sizeof(interrupt_error)-1u);halt();}if(!task_init()){serial_write_bytes(task_error,sizeof(task_error)-1u);halt();}serial_write_bytes(ready,sizeof(ready)-1u);protocol_loop();}
