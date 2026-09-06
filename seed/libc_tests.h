/* Empty namespace entries let this exercise a genuinely full table without
 * allocating all RAM. Rename must also work without a spare namespace slot. */
static int namespace_tests(void) {
    static const u8 src[]="test/fctl-source",dst[]="test/fctl-dest";
    u8 names[FMC][13];
    u64 flags=lock(),pages=mfc();
    u32 count=fc,made=0;
    int ok=0;
    if(ff(src,sizeof(src)-1)||ff(dst,sizeof(dst)-1))goto done;
    if(!fcreate(src,sizeof(src)-1)||!fw(src,sizeof(src)-1,0,(const u8 *)"payload",7))goto done;
    while(fc<FMC){
        const u8 *prefix=(const u8 *)"test/fctl-";
        for(u32 j=0;j<10;j++)names[made][j]=prefix[j];
        names[made][10]=(u8)('0'+made/100);
        names[made][11]=(u8)('0'+(made/10)%10);
        names[made][12]=(u8)('0'+made%10);
        if(!fcreate(names[made],13))goto done;
        ++made;
    }
    if(!made||fcreate(dst,sizeof(dst)-1)||
       !fmove(src,sizeof(src)-1,dst,sizeof(dst)-1)||fc!=FMC||
       ff(src,sizeof(src)-1))goto done;
    struct file *f=ff(dst,sizeof(dst)-1);
    if(!f||fz(f)!=7||fd(f)[0]!='p'||fd(f)[6]!='d')goto done;
    if(!fmove(dst,sizeof(dst)-1,names[0],13)||fc!=FMC-1||
       ff(dst,sizeof(dst)-1))goto done;
    f=ff(names[0],13);
    if(!f||fz(f)!=7||fd(f)[0]!='p'||fd(f)[6]!='d')goto done;
    ok=1;
done:
    if(ff(src,sizeof(src)-1)&&!fr(src,sizeof(src)-1))ok=0;
    if(ff(dst,sizeof(dst)-1)&&!fr(dst,sizeof(dst)-1))ok=0;
    for(u32 i=0;i<made;i++)if(!fr(names[i],13))ok=0;
    if(fc!=count||mfc()!=pages)ok=0;
    unlock(flags);return ok;
}
/* Boot-only integration test of separately compiled ordinary user files.
 * No fixture identity is consulted by the loader, scheduler or syscalls. */
#include "serial.h"
static int libc_tests(void) {
    if(!namespace_tests())return 0;
    static const u8 spin[]="seed/user/spin.cxe",report[]="seed/user/libtest.cxe";
    static const u8 result[]="test/rt-a",other[]="test/rt-b";
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
    if(state!=1 || status!=0 || slots[a].st!=RUN || *probe==before) goto done;
    b=-1;
    ok=1;
done:
    if(b>0) {
        if(active(&slots[b])) (void)tkill((u32)b);
        else (void)twait((u32)b,&status);
    }
    if(a>0 && !tkill((u32)a)) ok=0;
    if(ff(result,sizeof(result)-1) && !fr(result,sizeof(result)-1)) ok=0;
    if(ff(other,sizeof(other)-1) && !fr(other,sizeof(other)-1)) ok=0;
    if(mfc()!=free || fc!=count) ok=0;
    unlock(flags);
    if(!ok) {
        static const u8 msg[]="LIBC-TEST-FAIL phase=";
        swb(msg,sizeof(msg)-1); sw((u8)('0'+phase));
        sw(' '); sw((u8)('0'+((status/100)%10)));sw((u8)('0'+((status/10)%10)));sw((u8)('0'+(status%10))); sw('\n');
    } else {
        static const u8 msg[]="LIBC-USERLAND-PASS\n";
        swb(msg,sizeof(msg)-1);
    }
    return ok;
}
