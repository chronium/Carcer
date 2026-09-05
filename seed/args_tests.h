/* Boot-only integration: production launch paths, separately compiled programs,
 * independent non-syscalling CPU load, and resource accounting. */
static int args_tests(void) {
    static const u8 spin[]="seed/user/spin.cxe";
    static const u8 test[]="seed/user/argtest.cxe";
    static const u8 launcher[]="seed/user/launch.cxe";
    static const u8 config[]="runtime/launch.txt";
    static const u8 input[]="test/hash-input",output[]="test/hash-output";
    static const u8 description[]=
        "seed/user/sha256.cxe\ntest/hash-input\ntest/hash-output\n";
    static const u8 expected[7][66]={
        "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad\n",
        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n",
        "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0\n",
        "9f4390f8d30c2dd92ec9f095b65e2b9ae9b0a925a5258e241c9f1e910f734318\n",
        "b35439a4ac6f0948b6d6f9e3c6af0f5f590ce20f1bde7090ef7970686ec6738a\n",
        "7d3e74a05d7db15bce4ad9ec0658ea98e3f06eeecf16b4c6fff2da457ddc2f34\n",
        "ffe054fe7ae0cb6dc65c3af9b61d5209f439851db43d0ba5997337df154668eb\n"};
    u64 flags=lock(),free=mfc(),status=UINT64_MAX,before;
    u32 count=fc,phase=1;
    int busy=-1,task=-1,ok=0;
    volatile u64 *probe=0;
    if(ff(config,sizeof(config)-1) || ff(input,sizeof(input)-1) ||
       ff(output,sizeof(output)-1)) goto done;
    busy=tfile(spin,sizeof(spin)-1);
    if(busy<0) goto done;
    struct file *file=ff(spin,sizeof(spin)-1);
    const struct xh *h=(const void *)fd(file);
    const struct xs *v=(const void *)(fd(file)+sizeof(*h));
    for(u32 i=0;i<h->count;++i) if(v[i].flags&XW) {
        probe=(volatile u64 *)xl(slots[busy].cr3,v[i].va,1);break;
    }
    if(!probe) goto done;
    unlock(flags);
    u64 start=tnow();
    while(!*probe && tnow()-start<THZ) __asm__ volatile("pause");
    flags=lock();
    if(!*probe || slots[busy].st!=RUN) goto done;
    before=*probe;
    task=tfile(test,sizeof(test)-1);
    if(task<0) goto done;
    unlock(flags);
    int state=0;
    start=tnow();
    while(tnow()-start<5u*THZ) {
        state=twait((u32)task,&status);
        if(state) break;
        __asm__ volatile("pause");
    }
    flags=lock();phase=2;
    if(state!=1 || status || slots[busy].st!=RUN || *probe==before) goto done;
    task=-1;
    /* Exercise run-style no-argument launch -> user spawn_args -> hash -> wait.
     * Separate vectors include empty input and a multi-page streamed input. */
    if(!fw(config,sizeof(config)-1,0,description,sizeof(description)-1))
        goto done;
    for(u32 pass=0;pass<7;++pass) {
        u8 *data;
        static const u32 sizes[7]={3,0,1000000,55,56,63,64};
        u32 size=sizes[pass];
        if(!fws(input,sizeof(input)-1,0,size,&data)) goto done;
        for(u32 i=0;i<size;++i) data[i]=pass==0?(u8)('a'+i):'a';
        if(!ft(input,sizeof(input)-1,size)) goto done;
        before=*probe;
        task=tfile(launcher,sizeof(launcher)-1);
        if(task<0) goto done;
        unlock(flags);
        state=0;start=tnow();
        while(tnow()-start<5u*THZ) {
            state=twait((u32)task,&status);
            if(state) break;
            __asm__ volatile("pause");
        }
        flags=lock();phase=3;
        if(state!=1 || status || slots[busy].st!=RUN || *probe==before)
            goto done;
        task=-1;
        file=ff(output,sizeof(output)-1);
        if(!file || fz(file)!=65) goto done;
        for(u32 i=0;i<65;++i) if(fd(file)[i]!=expected[pass][i]) goto done;
        if(!fr(output,sizeof(output)-1)) goto done;
    }
    phase=4;
    static const u8 status_config[]="seed/user/argchild.cxe\ns\n";
    static const u8 fault_config[]="seed/user/argchild.cxe\nf\n";
    for(u32 pass=0;pass<2;++pass) {
        const u8 *desc=pass?fault_config:status_config;
        u64 want=pass?UINT64_MAX:0x1234567887654321ull;
        if(!ft(config,sizeof(config)-1,0) ||
           !fw(config,sizeof(config)-1,0,desc,sizeof(status_config)-1)) goto done;
        task=tfile(launcher,sizeof(launcher)-1);
        if(task<0) goto done;
        unlock(flags);
        int success=aw((u32)task,want);
        flags=lock();
        if(!success) goto done;
        task=-1;
    }
    /* Real parser/utility error paths also run as ordinary user workloads. */
    phase=5;
    static const u8 malformed[]="seed/user/sha256.cxe"; /* missing final LF */
    static const u8 missing[]="seed/user/sha256.cxe\ntest/no-input\ntest/hash-output\n";
    for(u32 pass=0;pass<2;++pass) {
        const u8 *desc=pass?missing:malformed;
        u32 length=pass?sizeof(missing)-1:sizeof(malformed)-1;
        if(!ft(config,sizeof(config)-1,0) ||
           !fw(config,sizeof(config)-1,0,desc,length)) goto done;
        task=tfile(launcher,sizeof(launcher)-1);
        if(task<0) goto done;
        unlock(flags);
        int success=aw((u32)task,pass?112:122);
        flags=lock();
        if(!success || ff(output,sizeof(output)-1)) goto done;
        task=-1;
    }
    if(!ft(config,sizeof(config)-1,0) ||
       !fw(config,sizeof(config)-1,0,description,sizeof(description)-1) ||
       !fw(output,sizeof(output)-1,0,(const u8 *)"keep",4)) goto done;
    task=tfile(launcher,sizeof(launcher)-1);
    if(task<0) goto done;
    unlock(flags);
    int refused=aw((u32)task,111);
    flags=lock();
    if(!refused) goto done;
    task=-1;
    file=ff(output,sizeof(output)-1);
    if(!file || fz(file)!=4 || fd(file)[0]!='k' || fd(file)[1]!='e' ||
       fd(file)[2]!='e' || fd(file)[3]!='p' || !fr(output,sizeof(output)-1))
        goto done;
    /* A valid argument launch with every user slot occupied fails without
     * allocating pages or disturbing any occupant. */
    phase=6;
    int occupied[NT];u32 used=0;
    for(u32 i=0;i<NT-2u;++i) {
        int id=tuser(workup,sizeof(workup),0);
        if(id<0) break;
        occupied[used++]=id;
    }
    struct argpack empty;
    empty.count=empty.used=0;
    u64 fullfree=mfc();
    int rejected=used==NT-2u && launchargs(test,sizeof(test)-1,&empty)<0 &&
                 mfc()==fullfree;
    while(used) if(!tkill((u32)occupied[--used])) rejected=0;
    if(!rejected) goto done;
    /* Fail each successive task-page allocation. This leaves all real free
     * pages available, avoiding destructive exhaustion of the 8 GiB machine.
     * Only this boot test changes the global budget, with interrupts disabled. */
    phase=7;
    static const u8 argchild[]="seed/user/argchild.cxe";
    u64 baseline=mfc();
    task=launchargs(argchild,sizeof(argchild)-1,&empty);
    if(task<0) goto done;
    u64 needed=baseline-mfc();
    if(!tkill((u32)task)) goto done;
    task=-1;
    if(!needed || mfc()!=baseline) goto done;
    for(u64 budget=0;budget<needed;++budget) {
        task_page_budget=budget;
        task=launchargs(argchild,sizeof(argchild)-1,&empty);
        task_page_budget=UINT64_MAX;
        if(task>=0 || mfc()!=baseline) goto done;
        for(u32 i=1;i<NT;++i)
            if((int)i!=busy && (slots[i].st || slots[i].cr3)) goto done;
    }
    task=launchargs(argchild,sizeof(argchild)-1,&empty);
    if(task<0) goto done;
    unlock(flags);
    int recovered=aw((u32)task,73);
    flags=lock();
    if(!recovered || mfc()!=baseline) goto done;
    task=-1;
    ok=1;
done:
    task_page_budget=UINT64_MAX;
    if(task>0) {
        if(active(&slots[task])) (void)tkill((u32)task);
        else (void)twait((u32)task,&status);
    }
    if(busy>0 && !tkill((u32)busy)) ok=0;
    /* On failure, children can still exist; fail boot rather than hide leaks. */
    if(ff(config,sizeof(config)-1) && !fr(config,sizeof(config)-1)) ok=0;
    if(ff(input,sizeof(input)-1) && !fr(input,sizeof(input)-1)) ok=0;
    if(ff(output,sizeof(output)-1) && !fr(output,sizeof(output)-1)) ok=0;
    if(mfc()!=free || fc!=count) ok=0;
    unlock(flags);
    if(!ok) {
        static const u8 message[]="ARGS-TEST-FAIL phase=";
        swb(message,sizeof(message)-1);sw((u8)('0'+phase));
        sw(' ');sw((u8)('0'+status%10));sw('\n');
    } else {
        static const u8 message[]="ARGS-USERLAND-PASS\n";
        swb(message,sizeof(message)-1);
    }
    return ok;
}
