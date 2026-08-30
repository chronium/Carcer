#include "memory.h"
#define LIMINE_MEMMAP_USABLE 0u
struct limine_memmap_entry {
uint64_t base;
uint64_t length;
uint64_t type;
};
struct limine_memmap_response {
uint64_t revision;
uint64_t entry_count;
struct limine_memmap_entry **entries;
};
struct limine_memmap_request {
uint64_t id[4];
uint64_t revision;
struct limine_memmap_response *response;
};
struct limine_hhdm_response {
uint64_t revision;
uint64_t offset;
};
struct limine_hhdm_request {
uint64_t id[4];
uint64_t revision;
struct limine_hhdm_response *response;
};
__attribute__((used, section(".limine_requests_start")))
static volatile uint64_t requests_start_marker[4] = {
0xf6b8f4b39de7d1aeull,
0xfab91a6940fcb9cfull,
0x785c6ed015d3e316ull,
0x181e920a7852b9d9ull
};
__attribute__((used, section(".limine_requests")))
static volatile uint64_t limine_base_revision[3] = {
0xf9562b2d5c95a6c8ull,
0x6a7b384944536bdcull,
3u
};
__attribute__((used, section(".limine_requests")))
static volatile struct limine_memmap_request memmap_request = {
{
0xc7b1dd30df4c8b88ull,
0x0a82e883a194f07bull,
0x67cf3d9d378a806full,
0xe304acdfc50c3c62ull
},
0u,
(struct limine_memmap_response *)0
};
__attribute__((used, section(".limine_requests")))
static volatile struct limine_hhdm_request hhdm_request = {
{
0xc7b1dd30df4c8b88ull,
0x0a82e883a194f07bull,
0x48dcf1cb8ad2b852ull,
0x63984e959a98244bull
},
0u,
(struct limine_hhdm_response *)0
};
__attribute__((used, section(".limine_requests_end")))
static volatile uint64_t requests_end_marker[2] = {
0xadc0e0531bb10d03ull,
0x9572709f31764c62ull
};
static struct limine_memmap_response *memory_map;
static uint8_t *allocation_bitmap;
static uint64_t direct_map_offset;
static uint64_t managed_page_count;
static uint64_t free_page_count;
static uint64_t bitmap_physical_base;
static uint64_t bitmap_page_count;
static int allocator_ready;
static uint64_t align_down(uint64_t value) {
return value & ~((uint64_t)MEMORY_PAGE_SIZE - 1u);
}
static int align_up(uint64_t value, uint64_t *result) {
uint64_t mask = (uint64_t)MEMORY_PAGE_SIZE - 1u;
if (value > 0xffffffffffffffffull - mask) {
return 0;
}
*result = (value + mask) & ~mask;
return 1;
}
static int range_end(uint64_t base, uint64_t length, uint64_t *end) {
if (length > 0xffffffffffffffffull - base) {
return 0;
}
*end = base + length;
return 1;
}
static int page_bit(uint64_t page) {
return (allocation_bitmap[page >> 3] & (uint8_t)(1u << (page & 7u))) != 0;
}
static void set_page_bit(uint64_t page) {
allocation_bitmap[page >> 3] |= (uint8_t)(1u << (page & 7u));
}
static void clear_page_bit(uint64_t page) {
allocation_bitmap[page >> 3] &= (uint8_t)~(1u << (page & 7u));
}
static int physical_page_is_usable(uint64_t physical_address) {
for (uint64_t index = 0; index < memory_map->entry_count; ++index) {
struct limine_memmap_entry *entry = memory_map->entries[index];
uint64_t end;
if (entry == (struct limine_memmap_entry *)0 ||
entry->type != LIMINE_MEMMAP_USABLE ||
!range_end(entry->base, entry->length, &end)) {
continue;
}
if (end >= MEMORY_PAGE_SIZE &&
physical_address >= entry->base &&
physical_address <= 0xffffffffffffffffull - MEMORY_PAGE_SIZE &&
physical_address + MEMORY_PAGE_SIZE <= end) {
return 1;
}
}
return 0;
}
void *memory_physical_to_virtual(uint64_t physical_address) {
if (!allocator_ready ||
physical_address > 0xffffffffffffffffull - direct_map_offset) {
return (void *)0;
}
return (void *)(uintptr_t)(physical_address + direct_map_offset);
}
uint64_t memory_pages_alloc(uint64_t page_count) {
if (!allocator_ready || page_count == 0 ||
page_count >= managed_page_count) {
return 0;
}
for (uint64_t first = 1; first < managed_page_count; ++first) {
if (page_count > managed_page_count - first) {
break;
}
uint64_t available = 0;
while (available < page_count && !page_bit(first + available)) {
++available;
}
if (available != page_count) {
first += available;
continue;
}
for (uint64_t index = 0; index < page_count; ++index) {
set_page_bit(first + index);
}
free_page_count -= page_count;
uint64_t physical_address = first * (uint64_t)MEMORY_PAGE_SIZE;
uint8_t *memory =
(uint8_t *)memory_physical_to_virtual(physical_address);
if (memory == (uint8_t *)0) {
for (uint64_t index = 0; index < page_count; ++index) {
clear_page_bit(first + index);
}
free_page_count += page_count;
return 0;
}
uint64_t byte_count = page_count * (uint64_t)MEMORY_PAGE_SIZE;
for (uint64_t offset = 0; offset < byte_count; ++offset) {
memory[offset] = 0;
}
return physical_address;
}
return 0;
}
uint64_t memory_page_alloc(void) {
return memory_pages_alloc(1);
}
int memory_pages_free(uint64_t physical_address, uint64_t page_count) {
if (!allocator_ready || page_count == 0 ||
(physical_address & ((uint64_t)MEMORY_PAGE_SIZE - 1u)) != 0 ||
physical_address == 0) {
return 0;
}
uint64_t first = physical_address / MEMORY_PAGE_SIZE;
if (first >= managed_page_count ||
page_count > managed_page_count - first) {
return 0;
}
uint64_t bitmap_first = bitmap_physical_base / MEMORY_PAGE_SIZE;
for (uint64_t index = 0; index < page_count; ++index) {
uint64_t page = first + index;
uint64_t address = physical_address +
index * (uint64_t)MEMORY_PAGE_SIZE;
if (!page_bit(page) || !physical_page_is_usable(address) ||
(page >= bitmap_first &&
page < bitmap_first + bitmap_page_count)) {
return 0;
}
}
for (uint64_t index = 0; index < page_count; ++index) {
clear_page_bit(first + index);
}
free_page_count += page_count;
return 1;
}
int memory_page_free(uint64_t physical_address) {
return memory_pages_free(physical_address, 1);
}
struct memory_stats memory_get_stats(void) {
struct memory_stats stats;
stats.total_pages = managed_page_count;
stats.free_pages = free_page_count;
stats.metadata_pages = bitmap_page_count;
return stats;
}
static int allocator_self_test(void) {
uint64_t first = memory_pages_alloc(2);
if (first == 0 || (first & (MEMORY_PAGE_SIZE - 1u)) != 0) {
return 0;
}
uint8_t *data = (uint8_t *)memory_physical_to_virtual(first);
if (data == (uint8_t *)0) {
return 0;
}
for (uint32_t offset = 0; offset < MEMORY_PAGE_SIZE * 2u; ++offset) {
if (data[offset] != 0) {
return 0;
}
data[offset] = 0xa5u;
}
return memory_pages_free(first, 2);
}
int memory_init(void) {
struct limine_memmap_response *map = memmap_request.response;
struct limine_hhdm_response *hhdm = hhdm_request.response;
uint64_t highest_address = 0;
allocator_ready = 0;
if (limine_base_revision[2] != 0 ||
map == (struct limine_memmap_response *)0 ||
hhdm == (struct limine_hhdm_response *)0 ||
map->entries == (struct limine_memmap_entry **)0 ||
map->entry_count == 0) {
return 0;
}
for (uint64_t index = 0; index < map->entry_count; ++index) {
struct limine_memmap_entry *entry = map->entries[index];
uint64_t end;
if (entry == (struct limine_memmap_entry *)0 ||
!range_end(entry->base, entry->length, &end)) {
return 0;
}
if (end > highest_address) {
highest_address = end;
}
}
uint64_t highest_page_address;
if (!align_up(highest_address, &highest_page_address) ||
highest_page_address == 0) {
return 0;
}
uint64_t page_count = highest_page_address / MEMORY_PAGE_SIZE;
uint64_t bitmap_bytes = (page_count + 7u) / 8u;
uint64_t bitmap_size;
if (!align_up(bitmap_bytes, &bitmap_size) || bitmap_size == 0) {
return 0;
}
uint64_t bitmap_base = 0;
for (uint64_t index = 0; index < map->entry_count; ++index) {
struct limine_memmap_entry *entry = map->entries[index];
uint64_t candidate;
uint64_t end;
if (entry->type != LIMINE_MEMMAP_USABLE ||
!align_up(entry->base, &candidate) ||
!range_end(entry->base, entry->length, &end) ||
candidate > end || bitmap_size > end - candidate) {
continue;
}
bitmap_base = candidate;
break;
}
if (bitmap_base == 0 ||
bitmap_base > 0xffffffffffffffffull - hhdm->offset) {
return 0;
}
memory_map = map;
direct_map_offset = hhdm->offset;
managed_page_count = page_count;
bitmap_physical_base = bitmap_base;
bitmap_page_count = bitmap_size / MEMORY_PAGE_SIZE;
allocation_bitmap =
(uint8_t *)(uintptr_t)(bitmap_physical_base + direct_map_offset);
for (uint64_t index = 0; index < bitmap_bytes; ++index) {
allocation_bitmap[index] = 0xffu;
}
free_page_count = 0;
for (uint64_t index = 0; index < map->entry_count; ++index) {
struct limine_memmap_entry *entry = map->entries[index];
uint64_t start;
uint64_t end;
if (entry->type != LIMINE_MEMMAP_USABLE ||
!align_up(entry->base, &start) ||
!range_end(entry->base, entry->length, &end)) {
continue;
}
end = align_down(end);
for (uint64_t address = start; address < end;
address += MEMORY_PAGE_SIZE) {
uint64_t page = address / MEMORY_PAGE_SIZE;
if (page < managed_page_count && page != 0 && page_bit(page)) {
clear_page_bit(page);
++free_page_count;
}
}
}
uint64_t bitmap_first_page = bitmap_physical_base / MEMORY_PAGE_SIZE;
for (uint64_t page = bitmap_first_page;
page < bitmap_first_page + bitmap_page_count;
++page) {
if (page < managed_page_count && !page_bit(page)) {
set_page_bit(page);
--free_page_count;
}
}
allocator_ready = 1;
if (free_page_count < 2 || !allocator_self_test()) {
allocator_ready = 0;
return 0;
}
return 1;
}
