/* Included by tasks.c: ordinary CXE2 fixtures plus boot-only observations.
 * No scheduler branches depend on these programs or on this test. */
extern const u8 wait_worker_end[];
__attribute__((naked,noinline,used)) static void wait_worker(void) {
    __asm__ volatile(
        "cmpq $1,0x401000\n"
        "je 4f\n"
        "cmpq $2,0x401000\n"
        "je 6f\n"
        /* Wait/reap client: arbitrary FP state must survive blocking. */
        "movabs $0x1234567887654321,%rbx\n"
        "movq %rbx,%xmm0\n"
        "punpcklqdq %xmm0,%xmm0\n"
        "movq %rbx,%xmm15\n"
        "punpcklqdq %xmm15,%xmm15\n"
        "movw $0x77f,0x401060\n"
        "fldcw 0x401060\n"
        "movl $0x3f80,0x401064\n"
        "ldmxcsr 0x401064\n"
        "fld1\n"
        "movq $1,0x401010\n"
        "1: incq 0x401040\n"
        "cmpq $0,0x401018\n"
        "je 1b\n"
        "movabs $0x1234567887654321,%rbx\n"
        "movabs $0x1234567976543211,%rcx\n"
        "movabs $0x1234567a76543212,%rdx\n"
        "movabs $0x1234567b76543213,%rbp\n"
        "movabs $0x1234567c76543214,%r8\n"
        "movabs $0x1234567d76543215,%r9\n"
        "movabs $0x1234567e76543216,%r10\n"
        "movabs $0x1234567f76543217,%r11\n"
        "movabs $0x1234568076543218,%r12\n"
        "movabs $0x1234568176543219,%r13\n"
        "movabs $0x123456827654321a,%r14\n"
        "movabs $0x123456837654321b,%r15\n"
        "mov %rsp,0x401058\n"
        "mov 0x401048,%rax\n"
        "mov 0x401008,%rdi\n"
        "mov 0x401030,%rsi\n"
        "int $0x80\n"
        "mov %rax,0x401020\n"
        "cmp %rsp,0x401058\n"
        "jne 9f\n"
        "cmp %rdi,0x401008\n"
        "jne 9f\n"
        "cmp %rsi,0x401030\n"
        "jne 9f\n"
        "movabs $0x1234567887654321,%rax\n"
        "cmp %rbx,%rax\n"
        "jne 9f\n"
        "movabs $0x1234567976543211,%rax\n"
        "cmp %rcx,%rax\n"
        "jne 9f\n"
        "movabs $0x1234567a76543212,%rax\n"
        "cmp %rdx,%rax\n"
        "jne 9f\n"
        "movabs $0x1234567b76543213,%rax\n"
        "cmp %rbp,%rax\n"
        "jne 9f\n"
        "movabs $0x1234567c76543214,%rax\n"
        "cmp %r8,%rax\n"
        "jne 9f\n"
        "movabs $0x1234567d76543215,%rax\n"
        "cmp %r9,%rax\n"
        "jne 9f\n"
        "movabs $0x1234567e76543216,%rax\n"
        "cmp %r10,%rax\n"
        "jne 9f\n"
        "movabs $0x1234567f76543217,%rax\n"
        "cmp %r11,%rax\n"
        "jne 9f\n"
        "movabs $0x1234568076543218,%rax\n"
        "cmp %r12,%rax\n"
        "jne 9f\n"
        "movabs $0x1234568176543219,%rax\n"
        "cmp %r13,%rax\n"
        "jne 9f\n"
        "movabs $0x123456827654321a,%rax\n"
        "cmp %r14,%rax\n"
        "jne 9f\n"
        "movabs $0x123456837654321b,%rax\n"
        "cmp %r15,%rax\n"
        "jne 9f\n"
        "fnstcw 0x401060\n"
        "cmpw $0x77f,0x401060\n"
        "jne 9f\n"
        "stmxcsr 0x401064\n"
        "cmpl $0x3f80,0x401064\n"
        "jne 9f\n"
        "movq %xmm0,%rax\n"
        "cmp %rbx,%rax\n"
        "jne 9f\n"
        "psrldq $8,%xmm0\n"
        "movq %xmm0,%rax\n"
        "cmp %rbx,%rax\n"
        "jne 9f\n"
        "movq %xmm15,%rax\n"
        "cmp %rbx,%rax\n"
        "jne 9f\n"
        "psrldq $8,%xmm15\n"
        "movq %xmm15,%rax\n"
        "cmp %rbx,%rax\n"
        "jne 9f\n"
        "fistpq 0x401050\n"
        "cmpq $1,0x401050\n"
        "jne 9f\n"
        "movq $2,0x401010\n"
        "2: incq 0x401040\n"
        "cmpq $2,0x401018\n"
        "jb 2b\n"
        "mov $42,%edi\n"
        "jmp 8f\n"
        /* Independent non-syscalling target/counter until explicitly released. */
        "4: movq $1,0x401010\n"
        "5: incq 0x401040\n"
        "cmpq $0,0x401018\n"
        "je 5b\n"
        "cmpq $2,0x401018\n"
        "je 7f\n"
        "mov $37,%edi\n"
        "jmp 8f\n"
        "6: movq $1,0x401010\n"
        "mov $11,%eax\n"
        "mov $1000,%edi\n"
        "int $0x80\n"
        "mov $37,%edi\n"
        "jmp 8f\n"
        "7: ud2\n"
        "9: mov $13,%edi\n"
        "8: xor %eax,%eax\n"
        "int $0x80\n"
        "ud2\n"
        ".global wait_worker_end\n"
        "wait_worker_end:\n"
    );
}
static int wtlaunch(u64 mode, u64 target, u64 gate, u64 call, u64 output) {
    static const u8 path[] = "test/wait-program.cxe";
    u8 image[2048];
    u32 code = (u32)(wait_worker_end - (const u8 *)wait_worker);
    if (code > sizeof(image) - 176u) return -1;
    (void)xi(image, (const u8 *)wait_worker, code);
    struct xs *segments = (void *)(image + sizeof(struct xh));
    u32 off = segments[1].off;
    segments[1].size = 88;
    for (u32 i = 0; i < 88; ++i) image[off + i] = 0;
    /* Byte copies avoid alignment assumptions about the file data offset. */
    u64 values[11] = {mode, target, 0, gate, 0xabcdef, 0x11223344,
                      output, 0, 0, call, 0};
    for (u32 i = 0; i < sizeof(values); ++i)
        image[off + i] = ((const u8 *)values)[i];
    u64 flags = lock();
    int id = -1;
    if (!ff(path, sizeof(path)-1) &&
        fw(path, sizeof(path)-1, 0, image, off + sizeof(values))) {
        id = tfile(path, sizeof(path)-1);
        if (!fr(path, sizeof(path)-1)) id = -1;
    }
    unlock(flags);
    return id;
}
static int wtget(int id, u32 offset, u64 *value) {
    u64 flags = lock();
    int ok = id > 0 && id < (int)NT && active(&slots[id]) &&
        gu(&slots[id], (u8 *)value, UB + PG + offset, 8);
    unlock(flags);
    return ok;
}
static int wtput(int id, u32 offset, u64 value) {
    u64 flags = lock();
    int ok = id > 0 && id < (int)NT && active(&slots[id]) &&
        pu(&slots[id], UB + PG + offset, (const u8 *)&value, 8);
    unlock(flags);
    return ok;
}
static int wtphase(int id, u64 phase) {
    u64 start = tnow(), value;
    while (tnow() - start < 2u * THZ) {
        if (!wtget(id, 16, &value)) return 0;
        if (value == phase) return 1;
        __asm__ volatile("pause");
    }
    return 0;
}
static int wtstate(int id, u8 state) {
    if (id <= 0 || id >= (int)NT) return 0;
    u64 start = tnow();
    while (tnow() - start < 2u * THZ) {
        u64 flags = lock();
        int found = slots[id].st == state;
        unlock(flags);
        if (found) return 1;
        __asm__ volatile("pause");
    }
    return 0;
}
static int wtresult(int id, u64 result, u64 status, int check_status) {
    u64 got, pointer;
    if (!wtphase(id, 2) || !wtget(id, 32, &got) || got != result) return 0;
    if (check_status) {
        if (!wtget(id, 48, &pointer)) return 0;
        u64 flags = lock();
        int ok = gu(&slots[id], (u8 *)&got, pointer, 8);
        unlock(flags);
        if (!ok || got != status) return 0;
    }
    return wtput(id, 24, 2) && aw((u32)id, 42);
}
static int wtprogress(int busy) {
    u64 before, after, start = tnow();
    if (!wtget(busy, 64, &before)) return 0;
    while (tnow() - start < 2u * THZ) {
        if (!wtget(busy, 64, &after)) return 0;
        if (after != before) return 1;
        __asm__ volatile("pause");
    }
    return 0;
}
static int wait_tests(void) {
    const u64 output = UB + PG + 40u;
    u64 free = mfc(), status, value;
    int busy = wtlaunch(1, 0, 0, 0, output);
    if (busy < 0 || !wtphase(busy, 1)) return 0;

    /* Sleepers are waitable; blocked clients retain their registers and pages.
     * A target result is reserved against both syscall and kernel reapers. */
    int target = wtlaunch(2, 0, 0, 0, output);
    int waiter = wtlaunch(0, (u64)target, 1, 12, 0x3fffeffCull);
    if (!wtstate(target, SLP) || !wtstate(waiter, WAI) ||
        twait((u32)target, &status) != -1 ||
        twait((u32)waiter, &status) != 0 ||
        !wtprogress(busy)) return 0;
    int other = wtlaunch(0, (u64)target, 1, 6, output);
    if (other < 0 || !wtresult(other, UINT64_MAX, 0x11223344, 1)) return 0;
    other = wtlaunch(0, (u64)target, 1, 12, output);
    if (other < 0 || !wtresult(other, UINT64_MAX, 0x11223344, 1)) return 0;
    if (!tkill((u32)target) || !wtresult(waiter, UINT64_MAX, 0, 0) ||
        twait((u32)target, &status) != -1) return 0;

    /* Normal exit and fault exit, with a destination spanning two stack pages.
     * Completion must consume the reserved zombie only after CR3 teardown. */
    for (u32 fault = 0; fault < 2; ++fault) {
        target = wtlaunch(1, 0, 0, 0, output);
        waiter = wtlaunch(0, (u64)target, 1, 12, 0x3fffeffCull);
        if (!wtstate(waiter, WAI) || !wtprogress(busy) ||
            !wtget(waiter, 64, &value)) return 0;
        u64 before = value;
        if (!wtprogress(busy) || !wtget(waiter, 64, &value) ||
            value != before || !wtput(target, 24, fault ? 2 : 1) ||
            !wtresult(waiter, 1, fault ? UINT64_MAX : 37, 1) ||
            twait((u32)target, &status) != -1) return 0;
    }

    /* Already completed targets take the immediate path. Invalid buffers must
     * not consume a zombie; the subsequent valid wait still gets its result. */
    target = wtlaunch(1, 0, 1, 0, output);
    if (!wtstate(target, ZOM)) return 0;
    waiter = wtlaunch(0, (u64)target, 1, 12, ST - 4);
    if (waiter < 0 || !wtresult(waiter, UINT64_MAX, 0, 0)) return 0;
    waiter = wtlaunch(0, (u64)target, 1, 12, output);
    if (waiter < 0 || !wtresult(waiter, 1, 37, 1)) return 0;

    /* Self, out-of-range, zero/kernel slot, and unmapped/RX destinations. */
    waiter = wtlaunch(0, 0, 0, 12, output);
    if (waiter < 0 || !wtput(waiter, 8, (u64)waiter) ||
        !wtput(waiter, 24, 1) ||
        !wtresult(waiter, UINT64_MAX, 0x11223344, 1)) return 0;
    static const u64 bad_targets[] = {0, NT, UINT64_MAX};
    for (u32 i = 0; i < sizeof(bad_targets)/sizeof(bad_targets[0]); ++i) {
        waiter = wtlaunch(0, bad_targets[i], 1, 12, output);
        if (waiter < 0 || !wtresult(waiter, UINT64_MAX, 0x11223344, 1))
            return 0;
    }
    static const u64 bad_outputs[] = {1, UB, ST - 4, UINT64_MAX};
    for (u32 i = 0; i < sizeof(bad_outputs)/sizeof(bad_outputs[0]); ++i) {
        waiter = wtlaunch(0, (u64)busy, 1, 12, bad_outputs[i]);
        if (waiter < 0 || !wtresult(waiter, UINT64_MAX, 0, 0) ||
            slots[busy].waiter) return 0;
    }

    /* Cancelling a blocked waiter releases its claim. A reused slot must never
     * receive the old wait's completion or retain its graph links. */
    target = wtlaunch(1, 0, 0, 0, output);
    waiter = wtlaunch(0, (u64)target, 1, 12, output);
    if (!wtstate(waiter, WAI) || !tkill((u32)waiter) ||
        twait((u32)target, &status) != 0) return 0;
    other = wtlaunch(1, 0, 0, 0, output);
    if (other != waiter || !wtput(target, 24, 1) ||
        !aw((u32)target, 37) || !wtget(other, 32, &value) ||
        value != 0xabcdef || !tkill((u32)other)) return 0;

    /* Cancelling a target wakes its waiter before target-slot reuse. */
    target = wtlaunch(1, 0, 0, 0, output);
    waiter = wtlaunch(0, (u64)target, 1, 12, output);
    if (!wtstate(waiter, WAI)) return 0;
    u64 flags = lock();
    int killed = tkill((u32)target);
    other = wtlaunch(1, 0, 0, 0, output);
    unlock(flags);
    if (!killed || other != target ||
        !wtresult(waiter, UINT64_MAX, 0x11223344, 1) ||
        !wtprogress(other) || !tkill((u32)other)) return 0;

    /* Three-task cycle rejection, then cancellation propagates through the
     * remaining chain without leaving claims or blocked tasks behind. */
    int a = wtlaunch(0, 0, 0, 12, output);
    int b = wtlaunch(0, 0, 0, 12, output);
    int c = wtlaunch(0, 0, 0, 12, output);
    if (a < 0 || b < 0 || c < 0 ||
        !wtput(a, 8, (u64)b) || !wtput(b, 8, (u64)c) ||
        !wtput(c, 8, (u64)a) || !wtput(a, 24, 1) ||
        !wtstate(a, WAI) || !wtput(b, 24, 1) ||
        !wtstate(b, WAI) || !wtput(c, 24, 1) ||
        !wtphase(c, 2) || !wtget(c, 32, &value) || value != UINT64_MAX ||
        !tkill((u32)b) || !wtresult(a, UINT64_MAX, 0x11223344, 1) ||
        !wtresult(c, UINT64_MAX, 0x11223344, 1)) return 0;

    if (!wtprogress(busy) || !tkill((u32)busy)) return 0;
    for (u32 i = 1; i < NT; ++i)
        if (slots[i].st || slots[i].cr3 || slots[i].waiter || slots[i].waitfor)
            return 0;
    return mfc() == free;
}
