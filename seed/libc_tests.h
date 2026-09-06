/* Empty namespace entries let this exercise a genuinely full table without
 * allocating all RAM. Rename must also work without a spare namespace slot. */
static int namespace_tests(void) {
    static const u8 src[]="test/fctl-source",dst[]="test/fctl-dest";
    u8 names[FMC][13];
    u64 flags=lock(),pages=mfc();
    u32 count=fc,made=0;
    u64 source_id=0,target_id=0;
    int ok=0;
    if(ff(src,sizeof(src)-1)||ff(dst,sizeof(dst)-1))goto done;
    if(!fcreate(src,sizeof(src)-1)||!fw(src,sizeof(src)-1,0,(const u8 *)"payload",7))goto done;
    source_id=fhold(ff(src,sizeof(src)-1));
    if(!source_id)goto done;
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
    target_id=fhold(ff(names[0],13));
    if(!target_id)goto done;
    if(!fmove(dst,sizeof(dst)-1,names[0],13)||fc!=FMC-1||
       ff(dst,sizeof(dst)-1))goto done;
    f=ff(names[0],13);
    if(!f||fz(f)!=7||fd(f)[0]!='p'||fd(f)[6]!='d'||f->id!=source_id||
       !fbyid(target_id)||fz(fbyid(target_id))||forphans()!=1)goto done;
    ok=1;
done:
    if(source_id&&!fdrop(source_id))ok=0;
    if(target_id&&!fdrop(target_id))ok=0;
    if(forphans())ok=0;
    if(ff(src,sizeof(src)-1)&&!fr(src,sizeof(src)-1))ok=0;
    if(ff(dst,sizeof(dst)-1)&&!fr(dst,sizeof(dst)-1))ok=0;
    for(u32 i=0;i<made;i++)if(!fr(names[i],13))ok=0;
    if(fc!=count||mfc()!=pages)ok=0;
    unlock(flags);return ok;
}

static u32 handle_count(u32 id) {
    u32 n=0;for(u32 i=0;i<FHC;i++)if(handles[id][i].token)++n;return n;
}
static int handles_empty(void) {
    for(u32 i=0;i<NT;i++)if(handle_count(i))return 0;
    for(u32 i=0;i<fc;i++)if(files[i].refs)return 0;
    return !forphans();
}
static int file_launch_mode(const char *mode) {
    static struct argpack a;
    const char *name="seed/user/libtest.cxe";
    a.count=2;a.used=0;
    const char *strings[2]={name,mode};
    for(u32 i=0;i<2;i++) {
        a.offset[i]=a.used;
        for(u32 j=0;;j++){u8 c=(u8)strings[i][j];a.data[a.used++]=c;if(!c)break;}
    }
    return launchargs((const u8 *)name,21,&a);
}
static int file_lifetimes(void) {
    static const u8 a[]="test/rt-a",b[]="test/rt-b";
    const char *modes[]={"leak-exit","leak-fault","leak-sleep","leak-spin","leak-owner","copy-handle","open-fail","copy-resume"};
    u64 flags=lock(),base=mfc(),status=0;
    u32 count=fc,phase=0;
    int busy=-1,client=-1,replacement=-1,mutator=-1,ok=0;
    u64 guard[128];u32 held=0;
    volatile u64 *counter=0;
    if(!handles_empty()||!fobject_tests())goto done;
    busy=tuser(workup,sizeof(workup),0);
    if(busy<0)goto done;
    counter=(volatile u64 *)xl(slots[busy].cr3,UB+256,1);
    if(!counter)goto done;
    u64 with_busy=mfc();
    for(phase=0;phase<8;phase++) {
        if(phase==6&&!fw(a,sizeof(a)-1,0,(const u8 *)"keep",4))goto done;
        u64 serial=handle_serial,expected=mfc();
        if(phase==6)handle_serial=UINT64_MAX-1;
        if(phase==5||phase==7){fxchecking=1;fxbad=fxprobes=0;}
        client=file_launch_mode(modes[phase]);
        if(client<0){handle_serial=serial;goto done;}
        u64 start=tnow(),before=*counter;
        int observed=0,state=0;
        u32 child=0;
        u64 destination=0;
        unlock(flags);
        while(tnow()-start<5u*THZ) {
            flags=lock();
            if(phase<2||phase==6) {
                state=twait((u32)client,&status);
                if(state==1){observed=1;break;}
            } else if(phase==5||phase==7) {
                if(slots[client].ir && !(slots[client].c.cs&3u)) {
                    if(handle_count((u32)client)!=1)break;
                    u8 *p=xl(slots[client].cr3,slots[client].hb,1);
                    if(!p)break;
                    u64 *entry=lf(slots[client].cr3,slots[client].hb,0);
                    if(!entry)break;
                    destination=*entry&PM;
                    observed=1;break;
                }
            } else {
                u8 wanted=phase==2?SLP:phase==4?WAI:RUN;
                if(slots[client].st==wanted && handle_count((u32)client)==16 &&
                   forphans()==(phase==4?2u:1u)) {
                    if(phase==4) {
                        for(u32 i=1;i<NT;i++)if(slots[i].owner==(u32)client&&
                           slots[i].job&&slots[i].st==RUN&&handle_count(i)==16)child=i;
                        if(!child){unlock(flags);continue;}
                    }
                    observed=1;break;
                }
            }
            unlock(flags);
            __asm__ volatile("pause");
        }
        /* Loop can leave with either lock state; reacquiring is safe. Preserve
         * original enabled flags for the next observation window. */
        __asm__ volatile("cli":::"memory");
        if(phase==6)handle_serial=serial;
        if(!observed)goto done;

        if(phase==7) {
            /* The reader was genuinely timer-preempted in ring0. Hold that
             * saved continuation while an ordinary user task moves the
             * namespace, then resume through the normal scheduler. */
            struct file *immutable=ff((const u8 *)"test/immutable",14);
            u32 old_index=(u32)(immutable-files);
            u64 identity=immutable->id;
            if(immutable->refs!=1||handles[client][0].pos!=2)goto done;
            slots[client].st=SLP;slots[client].wake=UINT64_MAX;
            mutator=file_launch_mode("namespace-move");
            if(mutator<0)goto done;
            u64 began=tnow();state=0;
            unlock(flags);
            while(tnow()-began<3u*THZ) {
                state=twait((u32)mutator,&status);
                if(state)break;
                __asm__ volatile("pause");
            }
            flags=lock();
            if(state!=1||status)goto done;
            mutator=-1;
            immutable=ff((const u8 *)"test/immutable",14);
            struct file *moved=ff((const u8 *)"0/fh-move",9);
            if(!immutable||immutable->id!=identity||immutable->refs!=1||
               immutable==files+old_index||files[old_index].id==identity||
               !moved||fz(moved)!=4||fd(moved)[0]!='m'||
               slots[client].st!=SLP||!slots[client].ir)goto done;
            fxchecking=0;slots[client].st=RUN;slots[client].wake=0;
            began=tnow();state=0;
            unlock(flags);
            while(tnow()-began<5u*THZ) {
                state=twait((u32)client,&status);
                if(state)break;
                __asm__ volatile("pause");
            }
            flags=lock();
            if(state!=1||status||!fr((const u8 *)"0/fh-move",9))goto done;
        }
        if(phase<2||phase==6||phase==7) {
            if(state!=1 || status!=(phase==1?UINT64_MAX:0))goto done;
        } else {
            if(!tkill((u32)client))goto done;
            if(child&&(slots[child].st||slots[child].cr3||handle_count(child)))goto done;
        }
        if(slots[client].st||slots[client].cr3||!handles_empty())goto done;
        if(mfc()!=expected)goto done;
        if(phase==5) {
            fxchecking=0;
            /* Reclaim and guard the actual copy destination page before task
             * slot reuse; a discarded continuation must never write again. */
            while(held<128) {
                u64 page=mpa();if(!page)goto done;
                guard[held++]=page;
                if(page==destination)break;
            }
            if(!held||guard[held-1]!=destination)goto done;
            u8 *protected=mpv(destination);
            for(u32 i=0;i<PG;i++)protected[i]=(u8)(0xa7u+i*13u);
            replacement=tuser(workup,sizeof(workup),0);
            if(replacement!=client)goto done;
            volatile u64 *progress=(volatile u64 *)xl(slots[replacement].cr3,UB+256,1);
            u64 earlier=*progress,now=tnow();
            unlock(flags);
            while(tnow()-now<10u)__asm__ volatile("pause");
            flags=lock();
            if(*progress==earlier||*counter==before||ff(b,sizeof(b)-1))goto done;
            for(u32 i=0;i<PG;i++)if(protected[i]!=(u8)(0xa7u+i*13u))goto done;
            if(!tkill((u32)replacement))goto done;
            replacement=-1;
            while(held)if(!mpf(guard[--held]))goto done;
        }
        client=-1;
        if(phase==6&&!fr(a,sizeof(a)-1))goto done;
        if(mfc()!=with_busy||fc!=count)goto done;
        u64 tick=tnow();
        unlock(flags);
        while(*counter==before&&tnow()-tick<THZ)__asm__ volatile("pause");
        flags=lock();
        if(*counter==before)goto done;
    }
    ok=1;
done:
    fxchecking=0;
    if(mutator>0) {
        if(active(&slots[mutator]))(void)tkill((u32)mutator);
        else (void)twait((u32)mutator,&status);
    }
    if(ff((const u8 *)"0/fh-move",9)&&!fr((const u8 *)"0/fh-move",9))ok=0;
    if(replacement>0&&active(&slots[replacement]))(void)tkill((u32)replacement);
    if(client>0) {
        if(active(&slots[client]))(void)tkill((u32)client);
        else (void)twait((u32)client,&status);
    }
    if(busy>0&&!tkill((u32)busy))ok=0;
    while(held)if(!mpf(guard[--held]))ok=0;
    if(ff(a,sizeof(a)-1)&&!fr(a,sizeof(a)-1))ok=0;
    if(ff(b,sizeof(b)-1)&&!fr(b,sizeof(b)-1))ok=0;
    if(mfc()!=base||fc!=count||!handles_empty())ok=0;
    unlock(flags);
    if(ok){static const u8 msg[]="FILE-HANDLE-LIFETIMES-PASS\n";swb(msg,sizeof(msg)-1);}
    else {
        static const u8 msg[]="FILE-HANDLE-FAIL phase=";swb(msg,sizeof(msg)-1);
        sw((u8)('0'+phase));sw(' ');
        sw((u8)('0'+status/100%10));sw((u8)('0'+status/10%10));sw((u8)('0'+status%10));sw('\n');
    }
    return ok;
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
    return ok&&file_lifetimes();
}
