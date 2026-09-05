/* Boot-only in-memory wire tests; no host job is submitted. They exercise
 * production framing, response handling and transactional import. */
struct fixture {
    u8 out[2048], input[2048];
    u32 outn, inn, at, bad;
};
static u8 test_read(void *ctx) {
    struct fixture *f=ctx;
    if(f->at>=f->inn) { f->bad=1; return 0; }
    return f->input[f->at++];
}
static void test_write(void *ctx,const u8 *p,u32 n) {
    struct fixture *f=ctx;
    if(n>sizeof(f->out)-f->outn) { f->bad=1; return; }
    for(u32 i=0;i<n;++i) f->out[f->outn++]=p[i];
}
static void test_snapshot(void *ctx,u16 n) {
    struct fixture *f=ctx;
    if(n) f->bad=1;
    static const u8 empty[2]={0,0};
    test_write(ctx,empty,2);
}
static void reset_fixture(struct fixture *f) {
    f->outn=f->inn=f->at=f->bad=0;
}
static void test_response(struct fixture *f,u32 id,u32 status,
                          const u8 *data,u32 n) {
    struct wire w={f,test_read,test_write,test_snapshot};
    f->outn=0;
    head(&w,HOST_SERVICE_RESPONSE,id,4u+n); put32(&w,status);
    test_write(f,data,n);
    f->inn=f->outn;
    for(u32 i=0;i<f->inn;++i) f->input[i]=f->out[i];
    f->outn=f->at=0;
}
static int bytes_equal(const u8 *a,const u8 *b,u32 n) {
    for(u32 i=0;i<n;++i) if(a[i]!=b[i]) return 0;
    return 1;
}
/* Streaming import peer generates arbitrary binary bytes without a MiB-sized
 * test buffer. It reads each production request's decimal offset and length,
 * and can deny/shorten a selected response or race the final publication. */
struct peer {
    struct fixture f;
    u32 requests, at, n, status, host, offset, fail_at, short_at;
    u32 collide_at;
    const u8 *path; u32 pn;
    u8 h[20];
};
static u8 pattern(u32 n) { return (u8)((n*73u)^(n>>8)); }
static void peer_write(void *ctx,const u8 *p,u32 n) {
    struct peer *s=ctx; test_write(&s->f,p,n);
}
static void peer_snapshot(void *ctx,u16 n) { (void)ctx; (void)n; }
static int peer_request(struct peer *s) {
    struct fixture *f=&s->f;
    const u8 *p=f->out;
    if(f->outn<20||!frame_valid(p)||get16(p+6)!=HOST_SERVICE_REQUEST||
       get32(p+12)!=f->outn-16) return 0;
    s->host=get32(p+8);
    u32 nn=get16(p+16),at=18;
    static const u8 name[]="read_bootstrap_artifact";
    if(nn!=sizeof(name)-1u||nn>f->outn-at||
       !bytes_equal(p+at,name,nn)) return 0;
    at+=nn;
    if(f->outn-at<2||get16(p+at)!=3) return 0;
    at+=2;
    struct arg a[3];
    for(u32 i=0;i<3;++i) {
        if(f->outn-at<4) return 0;
        a[i].n=get32(p+at); at+=4;
        if(a[i].n>f->outn-at) return 0;
        a[i].p=p+at; at+=a[i].n;
    }
    u64 off;
    if(at!=f->outn||!opaque(a[0].p,a[0].n)||
       !decimal(a[1].p,a[1].n,&off)||off>UINT32_MAX||
       !range(a[1].p,a[1].n,a[2].p,a[2].n,&s->n)) return 0;
    s->offset=(u32)off; ++s->requests;
    s->status=s->requests==s->fail_at?1:0;
    if(s->status) s->n=3;
    else if(s->requests==s->short_at&&s->n) --s->n;
    if(s->requests==s->collide_at) {
        static const u8 occupied[]={7,0,255};
        u64 flags=lock();
        if(!fw(s->path,s->pn,0,occupied,sizeof(occupied))) f->bad=1;
        unlock(flags);
    }
    f->outn=0;
    struct wire w={f,test_read,test_write,test_snapshot};
    head(&w,HOST_SERVICE_RESPONSE,s->host,4+s->n); put32(&w,s->status);
    for(u32 i=0;i<20;++i) s->h[i]=f->out[i];
    f->outn=0; s->at=0; return 1;
}
static u8 peer_read(void *ctx) {
    struct peer *s=ctx;
    if(s->at==20u+s->n) {
        if(!peer_request(s)) { s->f.bad=1; return 0; }
    }
    u32 at=s->at++;
    return at<20?s->h[at]:pattern(s->offset+at-20u);
}
static void reset_peer(struct peer *s,const u8 *p,u32 pn) {
    reset_fixture(&s->f);
    s->requests=s->n=s->status=s->host=s->offset=0;
    s->fail_at=s->short_at=s->collide_at=0;
    s->at=20; s->path=p; s->pn=pn;
}
static int remove_test(const u8 *p,u32 pn) {
    u64 f=lock(); int ok=fr(p,pn); unlock(f); return ok;
}
int boottest(void) {
    static struct fixture f;
    static struct peer s;
    struct wire w={&f,test_read,test_write,test_snapshot};
    struct wire peer={&s,peer_read,peer_write,peer_snapshot};
    u64 x; u32 z;
    static const u8 max[]="18446744073709551615",over[]="18446744073709551616";
    if(!decimal(max,20,&x)||x!=UINT64_MAX||decimal(over,20,&x)||
       decimal((const u8 *)"00",2,&x)||decimal((const u8 *)"-1",2,&x)||
       decimal((const u8 *)"",0,&x)||
       !range(max,20,(const u8 *)"0",1,&z)||z||
       range(max,20,(const u8 *)"1",1,&z)||
       !range((const u8 *)"0",1,(const u8 *)"1048576",7,&z)||z!=READ_MAX||
       range((const u8 *)"0",1,(const u8 *)"1048577",7,&z)) return 0;
    u8 ds[20];
    if(digits(UINT64_MAX,ds)!=20||!bytes_equal(ds,max,20)) return 0;
    static const u8 json[]="{\"version\":1}";
    static const u8 badutf[]={0xc0,0x80},nul[]={'x',0,'y'};
    if(!job_valid(json,sizeof(json)-1)||job_valid(0,1)||
       job_valid(nul,3)||job_valid(badutf,2)||job_valid(json,JOB_JSON_MAX+1u)||
       opaque(nul,3)||opaque(badutf,2)||opaque(json,256)) return 0;
    struct arg a={json,sizeof(json)-1};
    if(!envelope(13,1,&a,1,SS_SERIALIZED_MAX,&z)||
       z!=4+13+4+sizeof(json)-1+4+SS_SERIALIZED_MAX) return 0;
    a.n=UINT32_MAX;
    if(envelope(13,1,&a,1,2,&z)) return 0;
    reset_fixture(&f);
    job_request(&w,0x12345678,json,sizeof(json)-1,0,2);
    if(f.bad||f.outn!=16+4+13+4+sizeof(json)-1+4+2||
       !frame_valid(f.out)||get16(f.out+6)!=HOST_SERVICE_REQUEST||
       get32(f.out+8)!=0x12345678||get32(f.out+12)!=f.outn-16||
       get16(f.out+16)!=13||!bytes_equal(f.out+18,(const u8 *)"bootstrap_job",13)||
       get16(f.out+31)!=2||get32(f.out+33)!=sizeof(json)-1||
       !bytes_equal(f.out+37,json,sizeof(json)-1)||
       get32(f.out+37+sizeof(json)-1)!=2||
       f.out[f.outn-2]||f.out[f.outn-1]) return 0;
    f.out[4]=2;
    if(frame_valid(f.out)) return 0;
    static const u8 binary[]={0,255,128,13,10,0,42};
    reset_fixture(&f); test_response(&f,5,0,binary,sizeof(binary));
    relay(&w,5,9,1,sizeof(binary));
    if(f.bad||f.at!=f.inn||f.outn!=20+sizeof(binary)||
       get16(f.out+6)!=INVOKE_TOOL_RESPONSE||get32(f.out+8)!=9||
       get32(f.out+16)||!bytes_equal(f.out+20,binary,sizeof(binary))) return 0;
    /* Host diagnostics retain their status and exact bytes. */
    reset_fixture(&f); test_response(&f,5,2,binary,sizeof(binary));
    relay(&w,5,9,1,0);
    if(f.bad||f.at!=f.inn||get32(f.out+16)!=2||
       !bytes_equal(f.out+20,binary,sizeof(binary))) return 0;
    for(u32 mode=0;mode<4;++mode) {
        reset_fixture(&f);
        test_response(&f,mode==0?6:5,mode==1?3:0,binary,sizeof(binary));
        if(mode==2) f.input[6]=0;
        relay(&w,5,9,1,mode==3?6:sizeof(binary));
        if(f.bad||f.at!=f.inn||f.outn!=20||get32(f.out+16)!=1) return 0;
    }
    /* receive_exact must not touch output on short/long/denied reads. */
    u8 dest[8];
    for(u32 mode=0;mode<3;++mode) {
        for(u32 i=0;i<sizeof(dest);++i) dest[i]=77;
        reset_fixture(&f); test_response(&f,5,mode==2?1:0,binary,sizeof(binary));
        if(receive_exact(&w,5,dest,mode==0?6:8)||f.bad||f.at!=f.inn) return 0;
        for(u32 i=0;i<sizeof(dest);++i) if(dest[i]!=77) return 0;
    }
    static const u8 path[]="test/bootstrap-import",id[]="fixture-artifact";
    const u32 pn=sizeof(path)-1,in=sizeof(id)-1;
    u32 baseline=fc;
    u64 freepages=mfc();
    if(ff(path,pn)) return 0;
    /* A multi-chunk, non-page-sized binary import traverses the real staging,
     * request framing, exact response validation and file publication code. */
    reset_peer(&s,path,pn);
    const u32 total=READ_MAX+513;
    if(!import(&peer,id,in,path,pn,total)||s.f.bad||s.requests!=2) return 0;
    struct file *file=ff(path,pn);
    if(!file||fz(file)!=total) return 0;
    for(u32 i=0;i<total;++i) if(fd(file)[i]!=pattern(i)) return 0;
    reset_peer(&s,path,pn);
    if(import(&peer,id,in,path,pn,1)||s.requests) return 0;
    if(!remove_test(path,pn)) return 0;
    /* Later denial and short read discard all staging and publish nothing. */
    for(u32 mode=0;mode<2;++mode) {
        reset_peer(&s,path,pn);
        if(mode) s.short_at=2; else s.fail_at=2;
        if(import(&peer,id,in,path,pn,total)||s.f.bad||s.requests!=2||
           s.at!=20+s.n||ff(path,pn)||fc!=baseline) return 0;
    }
    reset_peer(&s,path,pn); s.collide_at=2;
    if(import(&peer,id,in,path,pn,total)||s.f.bad) return 0;
    file=ff(path,pn);
    if(!file||fz(file)!=3||fd(file)[0]!=7||fd(file)[1]||fd(file)[2]!=255||
       !remove_test(path,pn)) return 0;
    reset_peer(&s,path,pn); s.fail_at=1;
    if(import(&peer,id,in,path,pn,0)||s.f.bad||s.requests!=1||ff(path,pn)) return 0;
    reset_peer(&s,path,pn);
    if(!import(&peer,id,in,path,pn,0)||s.f.bad||s.requests!=1) return 0;
    file=ff(path,pn);
    if(!file||fz(file)||!remove_test(path,pn)||fc!=baseline||mfc()!=freepages) return 0;
    return 1;
}

/* Simulate a slow synchronous host response after scheduling is initialized.
 * The peer never yields or enters the scheduler explicitly. A non-syscalling
 * infinite user loop must not stop another user program from completing. */
struct slow_peer {
    struct fixture f;
    u8 waited;
    u64 flags, status;
    u32 a,b;
    int state,running;
};
static u8 slow_read(void *ctx) {
    struct slow_peer *s=ctx;
    if(!s->waited) {
        s->waited=1;
        /* Neither task could run before this reader: its caller created both
         * under an outer interrupt lock. Open and close the progress interval
         * here, and sample completion before returning any response byte. */
        u64 start=tnow();
        unlock(s->flags);
        while(tnow()-start<8) __asm__ volatile("pause");
        (void)lock();
        u64 unused=0;
        s->state=twait(s->b,&s->status);
        s->running=twait(s->a,&unused);
    }
    return test_read(&s->f);
}
static void slow_write(void *ctx,const u8 *p,u32 n) {
    struct slow_peer *s=ctx; test_write(&s->f,p,n);
}
int bootlive(void) {
    static struct slow_peer s;
    static const u8 spin[]={0xeb,0xfe};
    static const u8 done[]={0xbf,37,0,0,0,0x31,0xc0,0xcd,0x80};
    u64 flags=lock();
    if(!(flags&(1u<<9))) { unlock(flags); return 0; }
    u64 pages=mfc();
    int a=tuser(spin,sizeof(spin),0),b=tuser(done,sizeof(done),0);
    if(a<0||b<0) {
        if(a>=0) (void)tkill((u32)a);
        if(b>=0) (void)tkill((u32)b);
        unlock(flags); return 0;
    }
    reset_fixture(&s.f); s.waited=0;
    s.flags=flags; s.a=(u32)a; s.b=(u32)b;
    s.status=0; s.state=s.running=-1;
    static const u8 result[]={0,255,7};
    test_response(&s.f,123,0,result,sizeof(result));
    struct wire w={&s,slow_read,slow_write,0};
    relay(&w,123,124,1,sizeof(result));
    int killed=tkill((u32)a);
    if(s.state!=1) (void)tkill((u32)b);
    int ok=s.waited&&!s.f.bad&&s.f.at==s.f.inn&&
           s.f.outn==20+sizeof(result)&&get32(s.f.out+16)==0&&
           bytes_equal(s.f.out+20,result,sizeof(result))&&
           s.state==1&&s.status==37&&s.running==0&&killed&&mfc()==pages;
    unlock(flags);
    return ok;
}
