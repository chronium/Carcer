#include "memory.h"

#define LIMINE_MEMMAP_USABLE 0u
#define MEMORY_MAX_REGIONS 64u
#define NO_FREE_PAGE UINT64_MAX

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

struct memory_region {
    uint64_t start;
    uint64_t next;
    uint64_t end;
};

__attribute__((used, section(".limine_requests_start")))
static volatile uint64_t limine_requests_start_marker[4] = {
    0xf6b8f4b39de7d1ae,
    0xfab91a6940fcb9cf,
    0x785c6ed015d3e316,
    0x181e920a7852b9d9
};

__attribute__((used, section(".limine_requests")))
static volatile struct limine_memmap_request memmap_request = {
    {
        0xc7b1dd30df4c8b88,
        0x0a82e883a194f07b,
        0x67cf3d9d378a806f,
        0xe304acdfc50c3c62
    },
    0,
    (struct limine_memmap_response *)0
};

__attribute__((used, section(".limine_requests")))
static volatile struct limine_hhdm_request hhdm_request = {
    {
        0xc7b1dd30df4c8b88,
        0x0a82e883a194f07b,
        0x48dcf1cb8ad2b852,
        0x63984e959a98244b
    },
    0,
    (struct limine_hhdm_response *)0
};

__attribute__((used, section(".limine_requests_end")))
static volatile uint64_t limine_requests_end_marker[2] = {
    0xadc0e0531bb10d03,
    0x9572709f31764c62
};

static struct memory_region regions[MEMORY_MAX_REGIONS];
static uint32_t region_count;
static uint64_t hhdm_offset;
static uint64_t total_pages;
static uint64_t available_pages;
static uint64_t recycled_head = NO_FREE_PAGE;
static int initialized;

static uint64_t align_up_page(uint64_t value) {
    if (value > UINT64_MAX - (MEMORY_PAGE_SIZE - 1u)) {
        return UINT64_MAX;
    }
    return (value + MEMORY_PAGE_SIZE - 1u) &
           ~((uint64_t)MEMORY_PAGE_SIZE - 1u);
}

static uint64_t align_down_page(uint64_t value) {
    return value & ~((uint64_t)MEMORY_PAGE_SIZE - 1u);
}

static void swap_regions(struct memory_region *left, struct memory_region *right) {
    struct memory_region temporary = *left;
    *left = *right;
    *right = temporary;
}

static void sort_regions(void) {
    for (uint32_t index = 1; index < region_count; ++index) {
        uint32_t position = index;
        while (position > 0 &&
               regions[position].start < regions[position - 1u].start) {
            swap_regions(&regions[position], &regions[position - 1u]);
            --position;
        }
    }
}

static void merge_regions(void) {
    if (region_count == 0) {
        return;
    }

    uint32_t output = 0;
    for (uint32_t index = 1; index < region_count; ++index) {
        if (regions[index].start <= regions[output].end) {
            if (regions[index].end > regions[output].end) {
                regions[output].end = regions[index].end;
            }
        } else {
            ++output;
            regions[output] = regions[index];
        }
    }
    region_count = output + 1u;
}

static int page_belongs_to_allocated_region(uint64_t physical_address) {
    for (uint32_t index = 0; index < region_count; ++index) {
        if (physical_address >= regions[index].start &&
            physical_address < regions[index].next) {
            return 1;
        }
    }
    return 0;
}

void *memory_physical_to_virtual(uint64_t physical_address) {
    if (!initialized || physical_address > UINT64_MAX - hhdm_offset) {
        return (void *)0;
    }
    return (void *)(uintptr_t)(hhdm_offset + physical_address);
}

int memory_init(void) {
    struct limine_memmap_response *memmap = memmap_request.response;
    struct limine_hhdm_response *hhdm = hhdm_request.response;

    region_count = 0;
    total_pages = 0;
    available_pages = 0;
    recycled_head = NO_FREE_PAGE;
    initialized = 0;

    if (memmap == (struct limine_memmap_response *)0 ||
        hhdm == (struct limine_hhdm_response *)0 ||
        memmap->entries == (struct limine_memmap_entry **)0) {
        return 0;
    }
    hhdm_offset = hhdm->offset;

    for (uint64_t index = 0; index < memmap->entry_count; ++index) {
        struct limine_memmap_entry *entry = memmap->entries[index];
        if (entry == (struct limine_memmap_entry *)0 ||
            entry->type != LIMINE_MEMMAP_USABLE || entry->length == 0) {
            continue;
        }

        if (entry->length > UINT64_MAX - entry->base) {
            continue;
        }
        uint64_t raw_end = entry->base + entry->length;
        uint64_t start = align_up_page(entry->base);
        uint64_t end = align_down_page(raw_end);
        if (start == 0) {
            start = MEMORY_PAGE_SIZE;
        }
        if (start >= end) {
            continue;
        }
        if (region_count >= MEMORY_MAX_REGIONS) {
            /*
             * The bootstrap allocator is deliberately bounded. Ignoring
             * excess fragments wastes memory but preserves safe operation.
             */
            continue;
        }
        regions[region_count].start = start;
        regions[region_count].next = start;
        regions[region_count].end = end;
        ++region_count;
    }

    sort_regions();
    merge_regions();
    for (uint32_t index = 0; index < region_count; ++index) {
        regions[index].next = regions[index].start;
        uint64_t pages =
            (regions[index].end - regions[index].start) / MEMORY_PAGE_SIZE;
        if (pages > UINT64_MAX - total_pages) {
            return 0;
        }
        total_pages += pages;
    }
    if (total_pages == 0) {
        return 0;
    }

    available_pages = total_pages;
    initialized = 1;
    return 1;
}

uint64_t memory_alloc_page(void) {
    if (!initialized || available_pages == 0) {
        return 0;
    }

    if (recycled_head != NO_FREE_PAGE) {
        uint64_t result = recycled_head;
        uint64_t *link = (uint64_t *)memory_physical_to_virtual(result);
        if (link == (uint64_t *)0) {
            return 0;
        }
        recycled_head = *link;
        --available_pages;
        return result;
    }

    for (uint32_t index = 0; index < region_count; ++index) {
        if (regions[index].next < regions[index].end) {
            uint64_t result = regions[index].next;
            regions[index].next += MEMORY_PAGE_SIZE;
            --available_pages;
            return result;
        }
    }
    return 0;
}

int memory_free_page(uint64_t physical_address) {
    if (!initialized || physical_address == 0 ||
        (physical_address & (MEMORY_PAGE_SIZE - 1u)) != 0 ||
        !page_belongs_to_allocated_region(physical_address) ||
        available_pages == total_pages) {
        return 0;
    }

    uint64_t current = recycled_head;
    uint64_t visited = 0;
    while (current != NO_FREE_PAGE) {
        if (current == physical_address || visited >= total_pages) {
            return 0;
        }
        uint64_t *link = (uint64_t *)memory_physical_to_virtual(current);
        if (link == (uint64_t *)0) {
            return 0;
        }
        current = *link;
        ++visited;
    }

    uint64_t *link = (uint64_t *)memory_physical_to_virtual(physical_address);
    if (link == (uint64_t *)0) {
        return 0;
    }
    *link = recycled_head;
    recycled_head = physical_address;
    ++available_pages;
    return 1;
}

uint64_t memory_total_pages(void) {
    return total_pages;
}

uint64_t memory_free_pages(void) {
    return available_pages;
}
