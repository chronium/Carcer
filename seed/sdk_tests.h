/* Boot-only integration test of separately compiled ordinary user files.
 * No fixture identity is consulted by the loader, scheduler or syscalls. */
#include "serial.h"
static int sdk_tests(void) {
    static const u8 spin[]="seed/user/spin.cxe",report[]="seed/user/report.cxe";
    static const u8 result[]="test/sdk-result";
    u64 flags=lock(),free=mfc(),status=UINT64_MAX,before=0;
    u32 count=fc;
    int a=-1,b=-1,ok=0,phase=1;
    volatile u64 *probe=0;
    if(ff(result,sizeof(result)-1)) goto done;
    a=tfile(spin,sizeof(spin)-1);
    if(a<0) goto done;
    /* The spin fixture places its counter at the first writable segment's
     * start. Read its address from the CXE2 metadata, not an instruction offset. */
    struct file *file=ff(spin,sizeof(spin)-1);
    const struct xh *h=(const void *)fd(file);
    const struct xs *v=(const void *)(fd(file)+sizeof(*h));
    for(u32 i=0;i<h->count;i++) if(v[i].flags&XW) {
        probe=(volatile u64 *)xl(slots[a].cr3,v[i].va,1); break;
    }
    if(!probe || *probe) goto done;
    unlock(flags);
    u64 start=tnow();
    while(!*probe && tnow()-start<THZ) __asm__ volatile("pause");
    before=*probe;
    start=tnow();
    while(*probe==before && tnow()-start<THZ) __asm__ volatile("pause");
    flags=lock(); phase=2;
    if(!before || *probe==before || slots[a].st!=RUN) goto done;
    /* Only now start the unrelated workload, while the non-syscalling loop is
     * already making progress. All scheduling remains the ordinary PIT path. */
    before=*probe;
    b=tfile(report,sizeof(report)-1);
    if(b<0) goto done;
    unlock(flags);
    start=tnow();
    int state=0;
    while(tnow()-start<3u*THZ) {
        state=twait((u32)b,&status);
        if(state) break;
        __asm__ volatile("pause");
    }
    flags=lock(); phase=3;
    if(state!=1 || status!=42 || slots[a].st!=RUN || *probe==before) goto done;
    b=-1;
    file=ff(result,sizeof(result)-1);
    if(!file || fz(file)!=8193) goto done;
    for(u32 i=0;i<8193;i++)
        if(fd(file)[i]!=(u8)(i*17u+3u)) goto done;
    ok=1;
done:
    if(b>0) {
        if(active(&slots[b])) (void)tkill((u32)b);
        else (void)twait((u32)b,&status);
    }
    if(a>0 && !tkill((u32)a)) ok=0;
    if(ff(result,sizeof(result)-1) && !fr(result,sizeof(result)-1)) ok=0;
    if(mfc()!=free || fc!=count) ok=0;
    unlock(flags);
    if(!ok) {
        static const u8 msg[]="SDK-TEST-FAIL phase=";
        swb(msg,sizeof(msg)-1); sw((u8)('0'+phase));
        sw(' '); sw((u8)('0'+(status%10))); sw('\n');
    } else {
        static const u8 msg[]="SDK-USERLAND-PASS\n";
        swb(msg,sizeof(msg)-1);
    }
    return ok;
}
