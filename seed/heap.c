#include "heap.h"
#include "memory.h"
#define HEAP_ALIGNMENT 16u
#define HEAP_EXTENT_MAGIC 0x434f44455854454eull
#define HEAP_BLOCK_MAGIC 0x434f444558424c4bull
#define HEAP_BLOCK_FREE 0x46524545424c4f43ull
#define HEAP_BLOCK_USED 0x55534544424c4f43ull
struct heap_extent {
uint64_t magic;
uint64_t physical_base;
uint64_t page_count;
struct heap_extent *next;
};
struct heap_block {
uint64_t magic;
uint64_t capacity;
uint64_t requested_size;
uint64_t state;
};
static struct heap_extent *extent_list;
static uint64_t reserved_page_count;
static uint64_t active_allocation_count;
static uint64_t requested_byte_count;
static int heap_ready;
static uintptr_t align_address(uintptr_t address) {
return (address + HEAP_ALIGNMENT - 1u) &
~((uintptr_t)HEAP_ALIGNMENT - 1u);
}
static uint64_t align_size_down(uint64_t size) {
return size & ~((uint64_t)HEAP_ALIGNMENT - 1u);
}
static int align_size_up(uint64_t size, uint64_t *result) {
uint64_t mask = (uint64_t)HEAP_ALIGNMENT - 1u;
if (size > UINT64_MAX - mask) {
return 0;
}
*result = (size + mask) & ~mask;
return 1;
}
static uintptr_t extent_end(const struct heap_extent *extent) {
return (uintptr_t)extent +
extent->page_count * (uint64_t)MEMORY_PAGE_SIZE;
}
static struct heap_block *extent_first_block(struct heap_extent *extent) {
return (struct heap_block *)align_address(
(uintptr_t)extent + sizeof(struct heap_extent)
);
}
static uint8_t *block_payload(struct heap_block *block) {
return (uint8_t *)block + sizeof(struct heap_block);
}
static struct heap_block *block_next(
const struct heap_extent *extent,
struct heap_block *block
) {
uintptr_t next = (uintptr_t)block_payload(block) + block->capacity;
uintptr_t end = extent_end(extent);
if (next == end) {
return (struct heap_block *)0;
}
if (next > end || sizeof(struct heap_block) > end - next) {
return (struct heap_block *)0;
}
return (struct heap_block *)next;
}
static int block_is_valid(
const struct heap_extent *extent,
const struct heap_block *block
) {
uintptr_t address = (uintptr_t)block;
uintptr_t end = extent_end(extent);
if (block->magic != HEAP_BLOCK_MAGIC ||
(block->state != HEAP_BLOCK_FREE &&
block->state != HEAP_BLOCK_USED) ||
(block->capacity & (HEAP_ALIGNMENT - 1u)) != 0 ||
address > end ||
sizeof(struct heap_block) > end - address) {
return 0;
}
uintptr_t payload = address + sizeof(struct heap_block);
return block->capacity <= end - payload;
}
static struct heap_extent *create_extent(uint64_t aligned_size) {
uint64_t overhead = sizeof(struct heap_extent) + HEAP_ALIGNMENT - 1u +
sizeof(struct heap_block);
if (aligned_size > UINT64_MAX - overhead) {
return (struct heap_extent *)0;
}
uint64_t total = aligned_size + overhead;
if (total > UINT64_MAX - (MEMORY_PAGE_SIZE - 1u)) {
return (struct heap_extent *)0;
}
uint64_t pages =
(total + MEMORY_PAGE_SIZE - 1u) / MEMORY_PAGE_SIZE;
uint64_t physical_base = memory_pages_alloc(pages);
if (physical_base == 0) {
return (struct heap_extent *)0;
}
struct heap_extent *extent =
(struct heap_extent *)memory_physical_to_virtual(physical_base);
if (extent == (struct heap_extent *)0) {
(void)memory_pages_free(physical_base, pages);
return (struct heap_extent *)0;
}
extent->magic = HEAP_EXTENT_MAGIC;
extent->physical_base = physical_base;
extent->page_count = pages;
extent->next = extent_list;
struct heap_block *block = extent_first_block(extent);
uintptr_t payload = (uintptr_t)block_payload(block);
block->magic = HEAP_BLOCK_MAGIC;
block->capacity = align_size_down(extent_end(extent) - payload);
block->requested_size = 0;
block->state = HEAP_BLOCK_FREE;
if (block->capacity < aligned_size) {
(void)memory_pages_free(physical_base, pages);
return (struct heap_extent *)0;
}
extent_list = extent;
reserved_page_count += pages;
return extent;
}
static void *allocate_from_block(
struct heap_block *block,
uint64_t requested_size,
uint64_t aligned_size
) {
uint64_t remainder = block->capacity - aligned_size;
if (remainder >= sizeof(struct heap_block) + HEAP_ALIGNMENT) {
struct heap_block *split =
(struct heap_block *)(block_payload(block) + aligned_size);
split->magic = HEAP_BLOCK_MAGIC;
split->capacity = remainder - sizeof(struct heap_block);
split->requested_size = 0;
split->state = HEAP_BLOCK_FREE;
block->capacity = aligned_size;
}
block->requested_size = requested_size;
block->state = HEAP_BLOCK_USED;
++active_allocation_count;
requested_byte_count += requested_size;
return block_payload(block);
}
void *heap_alloc(uint64_t size) {
uint64_t aligned_size;
if (!heap_ready || size == 0 || !align_size_up(size, &aligned_size)) {
return (void *)0;
}
for (struct heap_extent *extent = extent_list;
extent != (struct heap_extent *)0;
extent = extent->next) {
if (extent->magic != HEAP_EXTENT_MAGIC) {
return (void *)0;
}
for (struct heap_block *block = extent_first_block(extent);
block != (struct heap_block *)0;
block = block_next(extent, block)) {
if (!block_is_valid(extent, block)) {
return (void *)0;
}
if (block->state == HEAP_BLOCK_FREE &&
block->capacity >= aligned_size) {
return allocate_from_block(block, size, aligned_size);
}
}
}
struct heap_extent *extent = create_extent(aligned_size);
if (extent == (struct heap_extent *)0) {
return (void *)0;
}
return allocate_from_block(
extent_first_block(extent),
size,
aligned_size
);
}
void *heap_calloc(uint64_t count, uint64_t size) {
if (count != 0 && size > UINT64_MAX / count) {
return (void *)0;
}
uint64_t total = count * size;
void *allocation = heap_alloc(total);
if (allocation == (void *)0) {
return (void *)0;
}
uint8_t *bytes = (uint8_t *)allocation;
for (uint64_t index = 0; index < total; ++index) {
bytes[index] = 0;
}
return allocation;
}
static struct heap_block *find_block(
void *pointer,
struct heap_extent **found_extent,
struct heap_extent **found_previous_extent,
struct heap_block **found_previous_block
) {
struct heap_extent *previous_extent = (struct heap_extent *)0;
for (struct heap_extent *extent = extent_list;
extent != (struct heap_extent *)0;
extent = extent->next) {
if (extent->magic != HEAP_EXTENT_MAGIC) {
return (struct heap_block *)0;
}
struct heap_block *previous_block = (struct heap_block *)0;
for (struct heap_block *block = extent_first_block(extent);
block != (struct heap_block *)0;
block = block_next(extent, block)) {
if (!block_is_valid(extent, block)) {
return (struct heap_block *)0;
}
if (block_payload(block) == (uint8_t *)pointer) {
*found_extent = extent;
*found_previous_extent = previous_extent;
*found_previous_block = previous_block;
return block;
}
previous_block = block;
}
previous_extent = extent;
}
return (struct heap_block *)0;
}
int heap_free(void *pointer) {
if (!heap_ready || pointer == (void *)0) {
return 0;
}
struct heap_extent *extent;
struct heap_extent *previous_extent;
struct heap_block *previous_block;
struct heap_block *block = find_block(
pointer,
&extent,
&previous_extent,
&previous_block
);
if (block == (struct heap_block *)0 ||
block->state != HEAP_BLOCK_USED) {
return 0;
}
requested_byte_count -= block->requested_size;
--active_allocation_count;
block->requested_size = 0;
block->state = HEAP_BLOCK_FREE;
struct heap_block *next = block_next(extent, block);
if (next != (struct heap_block *)0 &&
next->state == HEAP_BLOCK_FREE) {
block->capacity += sizeof(struct heap_block) + next->capacity;
}
if (previous_block != (struct heap_block *)0 &&
previous_block->state == HEAP_BLOCK_FREE) {
previous_block->capacity +=
sizeof(struct heap_block) + block->capacity;
block = previous_block;
}
struct heap_block *first = extent_first_block(extent);
if (block == first && block_next(extent, block) == (struct heap_block *)0) {
struct heap_extent *following_extent = extent->next;
uint64_t physical_base = extent->physical_base;
uint64_t pages = extent->page_count;
if (memory_pages_free(physical_base, pages)) {
if (previous_extent == (struct heap_extent *)0) {
extent_list = following_extent;
} else {
previous_extent->next = following_extent;
}
reserved_page_count -= pages;
}
}
return 1;
}
struct heap_stats heap_get_stats(void) {
struct heap_stats stats;
stats.reserved_pages = reserved_page_count;
stats.active_allocations = active_allocation_count;
stats.requested_bytes = requested_byte_count;
stats.free_bytes = 0;
for (struct heap_extent *extent = extent_list;
extent != (struct heap_extent *)0;
extent = extent->next) {
for (struct heap_block *block = extent_first_block(extent);
block != (struct heap_block *)0;
block = block_next(extent, block)) {
if (!block_is_valid(extent, block)) {
return stats;
}
if (block->state == HEAP_BLOCK_FREE) {
stats.free_bytes += block->capacity;
}
}
}
return stats;
}
static int heap_self_test(void) {
uint64_t free_pages_before = memory_get_stats().free_pages;
uint8_t *small = (uint8_t *)heap_alloc(1);
uint8_t *medium = (uint8_t *)heap_alloc(700);
uint8_t *large = (uint8_t *)heap_alloc(9000);
uint8_t *zeroed = (uint8_t *)heap_calloc(37, 13);
if (small == (uint8_t *)0 || medium == (uint8_t *)0 ||
large == (uint8_t *)0 || zeroed == (uint8_t *)0 ||
((uintptr_t)small & (HEAP_ALIGNMENT - 1u)) != 0 ||
((uintptr_t)medium & (HEAP_ALIGNMENT - 1u)) != 0 ||
((uintptr_t)large & (HEAP_ALIGNMENT - 1u)) != 0 ||
((uintptr_t)zeroed & (HEAP_ALIGNMENT - 1u)) != 0) {
return 0;
}
for (uint64_t index = 0; index < 37u * 13u; ++index) {
if (zeroed[index] != 0) {
return 0;
}
}
small[0] = 0x11u;
for (uint64_t index = 0; index < 700; ++index) {
medium[index] = (uint8_t)index;
}
for (uint64_t index = 0; index < 9000; ++index) {
large[index] = (uint8_t)(index >> 4);
}
if (!heap_free(medium) || !heap_free(small) ||
!heap_free(large) || !heap_free(zeroed) ||
heap_free(small)) {
return 0;
}
struct heap_stats stats = heap_get_stats();
return stats.reserved_pages == 0 &&
stats.active_allocations == 0 &&
stats.requested_bytes == 0 &&
memory_get_stats().free_pages == free_pages_before;
}
int heap_init(void) {
extent_list = (struct heap_extent *)0;
reserved_page_count = 0;
active_allocation_count = 0;
requested_byte_count = 0;
heap_ready = 1;
if (!heap_self_test()) {
heap_ready = 0;
return 0;
}
return 1;
}
