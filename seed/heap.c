#include "heap.h"
#include "memory.h"
#define HEAP_MAGIC 0x434f444558484541ull
struct heap_header {
uint64_t magic;
uint64_t pages;
uint64_t physical;
uint64_t size;
};
static int heap_ready;
void *heap_alloc(uint64_t size) {
uint64_t overhead=sizeof(struct heap_header)+MEMORY_PAGE_SIZE-1u;
if(!heap_ready||!size||size>UINT64_MAX-overhead) return (void *)0;
uint64_t pages=(size+overhead)/MEMORY_PAGE_SIZE;
uint64_t physical=memory_pages_alloc(pages);
if(!physical) return (void *)0;
struct heap_header *header=(struct heap_header *)memory_physical_to_virtual(physical);
if(!header) {
(void)memory_pages_free(physical,pages);
return (void *)0;
}
header->magic=HEAP_MAGIC;
header->pages=pages;
header->physical=physical;
header->size=size;
return header+1;
}
int heap_free(void *pointer) {
if(!heap_ready||!pointer) return 0;
struct heap_header *header=(struct heap_header *)pointer-1;
if(header->magic!=HEAP_MAGIC||!header->pages||header->physical==0||
memory_physical_to_virtual(header->physical)!=(void *)header) return 0;
uint64_t physical=header->physical;
uint64_t pages=header->pages;
header->magic=0;
if(memory_pages_free(physical,pages)) return 1;
header->magic=HEAP_MAGIC;
return 0;
}
int heap_init(void) {
heap_ready=1;
uint8_t *test=(uint8_t *)heap_alloc(17);
if(!test) {
heap_ready=0;
return 0;
}
for(uint32_t i=0;i<17;++i) test[i]=(uint8_t)i;
if(!heap_free(test)||heap_free(test)) {
heap_ready=0;
return 0;
}
return 1;
}
