/* Boot-only observations around separately compiled syscall clients. */
#include "serial.h"
static u64 job_probe_address(const u8 *path,u32 n) {
    struct file *file=ff(path,n);
    if(!file)return 0;
    const struct xh *h=(const void *)fd(file);
    const struct xs *v=(const void *)(fd(file)+sizeof(*h));
    for(u32 i=0;i<h->count;i++)if(v[i].flags&XW)return v[i].va;
    return 0;
}

/* Explicit barriers prevent later owner reuse/exit from hiding delayed cleanup. */
static int jtlaunch(const char *mode,const char *value) {
    static const u8 path[]="seed/user/jobtest.cxe";
    struct argpack a;
    const char *parts[3]={"jobtest",mode,value};
    a.count=value?3:2;a.used=0;
    for(u32 i=0;i<a.count;i++){
        a.offset[i]=a.used;
        const char *p=parts[i];
        do{a.data[a.used++]=(u8)*p;}while(*p++);
    }
    return launchargs(path,sizeof(path)-1,&a);
}
static int jtget(int id,u32 word,u64 *v) {
    static const u8 path[]="seed/user/jobtest.cxe";
    return id>0&&id<(int)NT&&active(&slots[id])&&
        gu(&slots[id],(u8 *)v,job_probe_address(path,sizeof(path)-1)+word*8,8);
}
static int jtput(int id,u32 word,u64 v) {
    static const u8 path[]="seed/user/jobtest.cxe";
    return id>0&&id<(int)NT&&active(&slots[id])&&
        pu(&slots[id],job_probe_address(path,sizeof(path)-1)+word*8,(const u8 *)&v,8);
}
static int jtphase(int id,u64 value) {
    u64 start=tnow(),v;
    while(tnow()-start<3u*THZ){
        u64 f=lock();int found=jtget(id,0,&v)&&v==value;unlock(f);
        if(found)return 1;
        __asm__ volatile("pause");
    }
    return 0;
}
static int jtfind(int owner,u64 handle) {
    for(u32 i=1;i<NT;i++)if(slots[i].st&&slots[i].owner==(u32)owner&&
        slots[i].job==handle)return (int)i;
    return -1;
}
static int jtcleared(int id) {
    return id>0&&id<(int)NT&&!slots[id].st&&!slots[id].cr3&&
        !slots[id].waiter&&!slots[id].waitfor;
}
static int job_cleanup_tests(void) {
    static const u8 spin[]="seed/user/spin.cxe";
    u64 f=lock(),free=mfc(),status=0;u32 count=fc;
    int legacy=-1,client=-1,ok=0,phase=1;u32 pass=0;
    volatile u64 *counter=0;
    legacy=tfile(spin,sizeof(spin)-1);
    if(legacy<0)goto done;
    counter=(volatile u64 *)xl(slots[legacy].cr3,job_probe_address(spin,sizeof(spin)-1),1);
    if(!counter)goto done;
    for(pass=0;pass<4;pass++){
        char value[4]={(char)('0'+pass),':',(char)('0'+legacy),0};
        client=jtlaunch("controlled",value);
        if(client<0)goto done;
        unlock(f);int reached=jtphase(client,10);f=lock();
        if(!reached)goto done;
        u64 baseline=mfc(),before=*counter;
        if(!jtput(client,7,1))goto done;
        unlock(f);reached=jtphase(client,11);f=lock();
        u64 handle,branch_handle,zombie_handle,grand_handle;
        if(!reached||!jtget(client,1,&handle))goto done;
        int owner=jtfind(client,handle);
        unlock(f);reached=jtphase(owner,1);f=lock();
        if(!reached||!jtget(owner,1,&branch_handle)||!jtget(owner,2,&zombie_handle))goto done;
        int branch=jtfind(owner,branch_handle),zombie=jtfind(owner,zombie_handle),grand=-1;
        phase=2;
        unlock(f);u64 start=tnow();int states=0;
        while(tnow()-start<3u*THZ){
            f=lock();
            if(jtget(branch,1,&grand_handle))grand=jtfind(branch,grand_handle);
            u64 progress=0;
            states=owner>0&&branch>0&&zombie>0&&grand>0&&
                active(&slots[owner])&&slots[branch].st==WAI&&
                slots[branch].waitfor==(u32)legacy&&slots[legacy].waiter==(u32)branch&&
                slots[grand].st==RUN&&jtget(grand,1,&progress)&&progress&&
                slots[zombie].st==ZOM&&!slots[zombie].cr3&&slots[zombie].xs==91;
            unlock(f);if(states)break;__asm__ volatile("pause");
        }
        f=lock();
        if(!states||!jtput(owner,7,1))goto done;
        phase=3;
        if(pass>=2){
            unlock(f);start=tnow();states=0;
            while(tnow()-start<3u*THZ){
                f=lock();u64 p=0;
                states=jtget(owner,0,&p)&&p==2&&
                    (pass==2?(slots[owner].st==WAI&&slots[owner].waitfor==(u32)branch&&
                               slots[branch].waiter==(u32)owner):
                              (slots[owner].st==SLP&&slots[owner].wake>tnow()+100));
                unlock(f);if(states)break;__asm__ volatile("pause");
            }
            f=lock();if(!states)goto done;
        }
        if(!jtput(client,7,2))goto done;
        unlock(f);reached=jtphase(client,12);f=lock();phase=4;
        /* Client is held before its next launch or exit. The old raw slots
         * and every wait edge must already be dead; legacy target survives. */
        if(!reached||!jtcleared(owner)||!jtcleared(branch)||!jtcleared(grand)||
           !jtcleared(zombie)||mfc()!=baseline||fc!=count||
           slots[legacy].st!=RUN||slots[legacy].waiter||slots[legacy].waitfor||
           *counter==before||!jtput(client,7,3))goto done;
        unlock(f);int result=aw((u32)client,0);f=lock();
        if(!result)goto done;
        client=-1;phase=1;
    }
    ok=1;
done:
    if(client>0){
        if(active(&slots[client]))(void)tkill((u32)client);
        else (void)twait((u32)client,&status);
    }
    if(legacy>0&&!tkill((u32)legacy))ok=0;
    if(mfc()!=free||fc!=count)ok=0;
    unlock(f);
    if(ok){static const u8 m[]="JOBS-IMMEDIATE-CLEANUP-PASS\n";swb(m,sizeof(m)-1);}
    else {static const u8 m[]="JOBS-CLEANUP-FAIL ";swb(m,sizeof(m)-1);sw((u8)('0'+pass));sw('/');sw((u8)('0'+phase));sw('\n');}
    return ok;
}
static int job_copy_cancel_test(void) {
    static const u8 spin[]="seed/user/spin.cxe",marker[]="test/job-resumed";
    u64 f=lock(),free=mfc(),status=0,held[128],dest=0,buffer=0;u32 count=fc,nheld=0;
    int legacy=-1,owner=-1,child=-1,dummy=-1,replacement=-1,ok=0,phase=1;
    volatile u64 *counter=0,*fresh=0;
    if(ff(marker,sizeof(marker)-1))goto done;
    struct file *immutable=ff((const u8 *)"test/immutable",14);
    if(!immutable||!(fa(immutable)&FIM)||fz(immutable)<16)goto done;
    legacy=tfile(spin,sizeof(spin)-1);
    if(legacy<0)goto done;
    counter=(volatile u64 *)xl(slots[legacy].cr3,job_probe_address(spin,sizeof(spin)-1),1);
    if(!counter)goto done;
    u64 baseline=mfc();
    owner=jtlaunch("copy-owner",0);
    if(owner<0)goto done;
    /* Existing boot-only FP probe stretches the normal immutable-read window.
     * Production scheduling, copying, cancellation and slot setup are used. */
    fxchecking=1;fxbad=fxprobes=0;
    unlock(f);u64 start=tnow();int suspended=0;
    while(tnow()-start<3u*THZ){
        f=lock();u64 handle;
        if(jtget(owner,1,&handle))child=jtfind(owner,handle);
        suspended=child>0&&slots[owner].st==WAI&&slots[owner].waitfor==(u32)child&&
            slots[child].waiter==(u32)owner&&slots[child].st==RUN&&slots[child].ir&&
            !(slots[child].c.cs&3u)&&jtget(child,2,&buffer);
        if(suspended){
            u64 *pte=lf(slots[child].cr3,buffer,0);
            if(pte)dest=*pte&PM;
            /* Stay interrupt-disabled through observation and destruction. */
            break;
        }
        unlock(f);__asm__ volatile("pause");
    }
    if(!suspended)f=lock();
    fxchecking=0;phase=2;
    int old_owner=owner,old_child=child;
    if(!suspended||!dest||ff(marker,sizeof(marker)-1)||!tkill((u32)owner))goto done;
    owner=-1;
    if(!jtcleared(old_owner)||!jtcleared(old_child)||mfc()!=baseline)goto done;
    /* Reclaim the exact former copy-destination page and guard its full bytes.
     * This is allocated memory belonging to the observer, not a dangling read. */
    while(nheld<128){
        u64 p=mpa();if(!p)goto done;
        held[nheld++]=p;
        volatile u64 *v=mpv(p);
        for(u32 i=0;i<PG/8;i++)v[i]=0x5aa55aa500000000ull+(u64)nheld*1024+i;
        if(p==dest)break;
    }
    if(!nheld||held[nheld-1]!=dest)goto done;
    dummy=tfile(spin,sizeof(spin)-1);replacement=jtlaunch("integrity",0);
    if(dummy!=old_owner||replacement!=old_child)goto done;
    static const u8 jobpath[]="seed/user/jobtest.cxe";
    fresh=(volatile u64 *)xl(slots[replacement].cr3,job_probe_address(jobpath,sizeof(jobpath)-1)+8,1);
    if(!fresh)goto done;
    u64 before=*counter,after=*fresh;phase=3;
    unlock(f);start=tnow();int progressed=0;
    while(tnow()-start<3u*THZ){
        f=lock();
        progressed=slots[legacy].st==RUN&&slots[replacement].st==RUN&&
            *counter!=before&&*fresh!=after&&tnow()-start>=10;
        unlock(f);if(progressed)break;__asm__ volatile("pause");
    }
    f=lock();
    if(!progressed||ff(marker,sizeof(marker)-1))goto done;
    for(u32 j=0;j<nheld;j++){
        volatile u64 *v=mpv(held[j]);
        for(u32 i=0;i<PG/8;i++)if(v[i]!=0x5aa55aa500000000ull+(u64)(j+1)*1024+i)goto done;
    }
    ok=1;
done:
    fxchecking=0;
    if(owner>0){
        if(active(&slots[owner]))(void)tkill((u32)owner);
        else (void)twait((u32)owner,&status);
    }
    if(replacement>0){
        if(active(&slots[replacement])){if(!tkill((u32)replacement))ok=0;}
        else {ok=0;(void)twait((u32)replacement,&status);}
    }
    if(dummy>0&&!tkill((u32)dummy))ok=0;
    if(legacy>0&&!tkill((u32)legacy))ok=0;
    while(nheld)if(!mpf(held[--nheld]))ok=0;
    if(ff(marker,sizeof(marker)-1)){ok=0;(void)fr(marker,sizeof(marker)-1);}
    if(mfc()!=free||fc!=count)ok=0;
    unlock(f);
    if(ok){static const u8 m[]="JOBS-COPY-CANCEL-PASS\n";swb(m,sizeof(m)-1);}
    else {static const u8 m[]="JOBS-COPY-CANCEL-FAIL ";swb(m,sizeof(m)-1);sw((u8)('0'+phase));sw('\n');}
    return ok;
}

static int job_tests(void) {
    static const u8 spin[]="seed/user/spin.cxe",path[]="seed/user/jobtest.cxe";
    static const u8 ready_path[]="test/job-ready";
    u64 flags=lock(),free=mfc(),status=UINT64_MAX,before=0;
    u32 count=fc;
    int a=-1,b=-1,ok=0,phase=1,injected=0,legacy=0,saw_wait=0,saw_sleep=0,saw_tree=0;
    volatile u64 *counter=0;
    u64 address=job_probe_address(path,sizeof(path)-1);
    a=tfile(spin,sizeof(spin)-1);
    if(a<0||!address)goto done;
    counter=(volatile u64 *)xl(slots[a].cr3,job_probe_address(spin,sizeof(spin)-1),1);
    if(!counter)goto done;
    unlock(flags);
    u64 start=tnow();
    while(!*counter&&tnow()-start<THZ)__asm__ volatile("pause");
    before=*counter;start=tnow();
    while(*counter==before&&tnow()-start<THZ)__asm__ volatile("pause");
    flags=lock();
    if(!before||*counter==before)goto done;
    before=*counter;b=tfile(path,sizeof(path)-1);
    if(b<0)goto done;
    unlock(flags);start=tnow();
    int state=0;
    while(tnow()-start<20u*THZ) {
        flags=lock();
        if(active(&slots[b])){
            u64 p[4];
            if(!gu(&slots[b],(u8 *)p,address,sizeof(p))){unlock(flags);break;}
            if(p[0]==1&&!injected){
                for(u32 i=1;i<NT;i++)if(slots[i].st&&slots[i].job==p[1]&&slots[i].owner==(u32)b){
                    u64 raw=i;injected=pu(&slots[b],address+16,(const u8 *)&raw,8);break;
                }
            }
            if(p[3])legacy=1;
        }
        for(u32 i=1;i<NT;i++)if(slots[i].st&&slots[i].job){
            u64 sentinel=0xabc;
            if(twait(i,&sentinel)!=-1||sentinel!=0xabc){unlock(flags);goto failed_loop;}
            if(slots[i].st==WAI)saw_wait=1;
            if(slots[i].st==SLP)saw_sleep=1;
            if(slots[i].owner&&slots[slots[i].owner].job)saw_tree=1;
        }
        state=twait((u32)b,&status);
        unlock(flags);
        if(state)break;
        __asm__ volatile("pause");
    }
failed_loop:
    flags=lock();phase=2;
    if(state!=1||status||!injected||!legacy||!saw_wait||!saw_sleep||!saw_tree||
       slots[a].st!=RUN||*counter==before)goto done;
    b=-1;
    for(u32 i=1;i<NT;i++)if(i!=(u32)a&&slots[i].st)goto done;
    if(!tkill((u32)a))goto done;
    a=-1;
    if(mfc()!=free||fc!=count)goto done;
    /* Failed allocations must neither publish a child nor spend a handle.
     * Exhaustion is injected without issuing any token; restore afterwards. */
    phase=3;
    for(u32 pass=0;pass<4;pass++){
        struct argpack args;
        static const u8 strings[]="jobtest\0allocation";
        args.count=2;args.used=sizeof(strings);args.offset[0]=0;args.offset[1]=8;
        for(u32 i=0;i<sizeof(strings);i++)args.data[i]=strings[i];
        b=launchargs(path,sizeof(path)-1,&args);
        if(b<0)goto done;
        u64 serial=job_serial;
        if(pass==3)job_serial=UINT64_MAX-1;
        else task_page_budget=pass==0?0:pass==1?1:3;
        unlock(flags);
        int result=aw((u32)b,0);
        flags=lock();
        task_page_budget=UINT64_MAX;
        int unchanged=job_serial==(pass==3?UINT64_MAX-1:serial);
        job_serial=serial;
        if(!result||!unchanged)goto done;
        b=-1;
        if(mfc()!=free||fc!=count)goto done;
    }
    phase=4;
    unlock(flags);int extra=job_cleanup_tests()&&job_copy_cancel_test();flags=lock();
    if(!extra)goto done;
    ok=1;
done:
    task_page_budget=UINT64_MAX;
    if(b>0){
        if(active(&slots[b]))(void)tkill((u32)b);
        else (void)twait((u32)b,&status);
    }
    if(a>0&&!tkill((u32)a))ok=0;
    if(ff(ready_path,sizeof(ready_path)-1)&&!fr(ready_path,sizeof(ready_path)-1))ok=0;
    if(mfc()!=free||fc!=count)ok=0;
    unlock(flags);
    if(ok){static const u8 m[]="SUPERVISED-JOBS-PASS\n";swb(m,sizeof(m)-1);}
    else {
        static const u8 m[]="SUPERVISED-JOBS-FAIL phase=";swb(m,sizeof(m)-1);
        sw((u8)('0'+phase));sw(' ');
        sw((u8)('0'+status/100%10));sw((u8)('0'+status/10%10));sw((u8)('0'+status%10));sw('\n');
    }
    return ok;
}
