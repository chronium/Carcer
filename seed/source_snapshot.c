#pragma GCC target("general-regs-only")
#include "source_snapshot.h"
#include "files.h"
#include "protocol.h"
#include "serial.h"

/* Only safe seed/ paths participate in the persistent source snapshot. */
static int valid(const struct file *f) {
    static const uint8_t prefix[] = "seed/";
    uint32_t start = sizeof(prefix) - 1u, n = f->n;
    if (n <= start || n > FPL) return 0;
    for (uint32_t i = 0; i < start; ++i)
        if (f->p[i] != prefix[i]) return 0;
    for (uint32_t i = 0; i < n; ++i)
        if (!f->p[i]) return 0;
    for (uint32_t i = start; i <= n; ++i) {
        if (i == n || f->p[i] == '/') {
            uint32_t len = i - start;
            if (!len || (len == 1 && f->p[start] == '.') ||
                (len == 2 && f->p[start] == '.' && f->p[start + 1] == '.'))
                return 0;
            start = i + 1u;
        }
    }
    return 1;
}

static int measure(const struct file *list, uint32_t count,
                   uint16_t *out_count, uint32_t *out_bytes) {
    uint16_t included = 0;
    uint32_t content = 0, bytes = 2;
    if (count > FMC) return 0;
    for (uint32_t i = 0; i < count; ++i) {
        const struct file *f = list + i;
        if (!valid(f)) continue;
        uint32_t size = fz(f), framing = 6u + f->n;
        if (size > SS_CONTENT_MAX - content ||
            framing > SS_SERIALIZED_MAX - bytes ||
            size > SS_SERIALIZED_MAX - bytes - framing)
            return 0;
        content += size;
        bytes += framing + size;
        ++included;
    }
    *out_count = included;
    *out_bytes = bytes;
    return 1;
}

int ssmeasure(uint16_t *count, uint32_t *bytes) {
    return measure(files, fc, count, bytes);
}

void sswrite(uint16_t count) {
    pw16(count);
    for (uint32_t i = 0; i < fc; ++i) {
        const struct file *f = files + i;
        if (!valid(f)) continue;
        pw16(f->n);
        swb(f->p, f->n);
        pw32(fz(f));
        swb(fd(f), fz(f));
    }
}

/* Metadata boundary tests: measurement never dereferences file contents. */
int sstest(void) {
    static struct file fixtures[FMC];
    uint16_t count;
    uint32_t bytes;
    for (uint32_t i = 0; i < FMC; ++i) {
        struct file *f = fixtures + i;
        f->n = FPL;
        for (uint32_t j = 0; j < FPL; ++j) f->p[j] = 'x';
        f->p[0] = 's'; f->p[1] = 'e'; f->p[2] = 'e';
        f->p[3] = 'd'; f->p[4] = '/';
        f->p[FPL - 2] = (uint8_t)('A' + i / 16u);
        f->p[FPL - 1] = (uint8_t)('A' + i % 16u);
        f->z = SS_CONTENT_MAX / FMC;
    }
    if (!measure(fixtures, FMC, &count, &bytes) ||
        count != FMC || bytes != SS_SERIALIZED_MAX) return 0;
    ++fixtures[0].z;
    if (measure(fixtures, FMC, &count, &bytes)) return 0;
    --fixtures[0].z;
    if (measure(fixtures, FMC + 1u, &count, &bytes)) return 0;
    fixtures[0].z = 65537;
    if (!measure(fixtures, 1, &count, &bytes) ||
        count != 1 || bytes != 65537u + 6u + FPL + 2u) return 0;
    fixtures[0].z = UINT32_MAX;
    if (measure(fixtures, 1, &count, &bytes)) return 0;
    fixtures[0].p[0] = 't';
    if (!measure(fixtures, 1, &count, &bytes) || count || bytes != 2)
        return 0;
    fixtures[0].p[0] = 's';
    fixtures[0].p[5] = '.'; fixtures[0].p[6] = '.';
    fixtures[0].n = 7;
    if (!measure(fixtures, 1, &count, &bytes) || count || bytes != 2)
        return 0;
    return ssmeasure(&count, &bytes);
}
