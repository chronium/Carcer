/* Boot-only integration test of separately compiled ordinary user files.
 * No fixture identity is consulted by the loader, scheduler or syscalls. */
#include "serial.h"
static int console_tests(void) {
    static const u8 spin[]="seed/user/spin.cxe",report[]="seed/user/consoletest.cxe";
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
    while(tnow()-start<30u*THZ) {
        state=twait((u32)b,&status);
        if(state) break;
        __asm__ volatile("pause");
    }
    flags=lock(); phase=3;
    if(state!=1 || status!=0 || slots[a].st!=RUN || *probe==before) goto done;
    b=-1;
    {
        static const u8 frame[]="test/console-frame";
        struct file *captured=ff(frame,sizeof(frame)-1);
        struct vinfo info;vinfo(&info);
        u32 w=(info.width<800?info.width:800)/6*6;
        u32 h=(info.height<480?info.height:480)/8*8,pitch;
        u8 *vga=vtarget(0,0,w,h,&pitch);
        if(!captured || fz(captured)!=(u64)w*h*4 || !vga)goto done;
        for(u32 y=0;y<h;y++)for(u32 x=0;x<w*4;x++)
            if(vga[(u64)y*pitch+x]!=fd(captured)[(u64)y*w*4+x])goto done;
    }
    ok=1;
done:
    if(b>0) {
        if(active(&slots[b])) (void)tkill((u32)b);
        else (void)twait((u32)b,&status);
    }
    if(a>0 && !tkill((u32)a)) ok=0;
    if(ff(result,sizeof(result)-1) && !fr(result,sizeof(result)-1)) ok=0;
    if(ff(other,sizeof(other)-1) && !fr(other,sizeof(other)-1)) ok=0;
    static const char *cleanup[]={"test/con-script","test/con-log","test/con file","test/con-moved","test/con-bad","test/console-frame","test/con-fill","test/con-pad","test/con-target"};
    for(u32 i=0;i<sizeof(cleanup)/sizeof(cleanup[0]);i++){
        const u8 *p=(const u8 *)cleanup[i];u32 n=0;while(p[n])n++;
        if(ff(p,n)&&!fr(p,n))ok=0;
    }
    if(mfc()!=free || fc!=count) ok=0;
    unlock(flags);
    if(!ok) {
        static const u8 msg[]="CONSOLE-TEST-FAIL phase=";
        swb(msg,sizeof(msg)-1); sw((u8)('0'+phase));
        sw(' '); sw((u8)('0'+((status/100)%10)));sw((u8)('0'+((status/10)%10)));sw((u8)('0'+(status%10))); sw('\n');
    } else {
        static const u8 msg[]="CONSOLE-USERLAND-PASS\n";
        swb(msg,sizeof(msg)-1);
    }
    return ok;
}
