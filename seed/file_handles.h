/* Included by tasks.c; all entry paths hold the single CPU interrupt lock. */
#define FO_READ 1u
#define FO_WRITE 2u
#define FO_CREATE 4u
#define FO_EXCL 8u
#define FO_TRUNC 16u
#define FO_APPEND 32u
static struct tc *file_open(struct tc *f) {
    struct ts *s=&slots[cur];
    u8 path[FPL];
    u64 flags=f->rdx;
    u32 at=0;
    f->rax=UINT64_MAX;
    if(!gp(s,f,path) || flags>63 || !(flags&3) ||
       ((flags&(FO_CREATE|FO_TRUNC|FO_APPEND)) && !(flags&FO_WRITE)) ||
       ((flags&FO_EXCL) && !(flags&FO_CREATE)) ||
       handle_serial==UINT64_MAX-1)return f;
    while(at<FHC && handles[cur][at].token)++at;
    if(at==FHC)return f;
    struct file *file=ff(path,(u32)f->rsi);
    if(file) {
        if((flags&FO_EXCL) || ((flags&FO_WRITE)&&(fa(file)&FIM)) ||
           file->refs==UINT32_MAX)return f;
    } else {
        if(!(flags&FO_CREATE) || !fcreate(path,(u32)f->rsi))return f;
        file=ff(path,(u32)f->rsi);
    }
    u64 identity=fhold(file);
    if(!identity)return f;
    if((flags&FO_TRUNC) && !fresize(file,0)) {
        (void)fdrop(identity);return f;
    }
    struct open_file *h=&handles[cur][at];
    h->identity=identity;h->pos=(flags&FO_APPEND)?fz(file):0;
    h->flags=(u32)flags;h->token=++handle_serial;
    f->rax=h->token;return f;
}
static struct tc *file_handle(struct tc *f) {
    struct ts *s=&slots[cur];
    u64 token=f->rdi,op=f->rsi;
    struct open_file *h=0;
    f->rax=UINT64_MAX;
    if(!token || token==UINT64_MAX || op>5)return f;
    for(u32 i=0;i<FHC;i++)if(handles[cur][i].token==token) {
        h=&handles[cur][i];break;
    }
    if(!h)return f;
    struct file *file=fbyid(h->identity);
    if(!file)return f;
    if(op==0) {
        if(fdrop(h->identity)){h->token=0;f->rax=0;}
        return f;
    }
    if(op==3) {
        u64 base,magnitude;
        if(f->rcx==0)base=0;
        else if(f->rcx==1)base=h->pos;
        else if(f->rcx==2)base=fz(file);
        else return f;
        int negative=(f->rdx>>63)!=0;
        magnitude=negative?0ull-f->rdx:f->rdx;
        if((negative && magnitude>base) ||
           (!negative && magnitude>UINT32_MAX-base))return f;
        h->pos=negative?base-magnitude:base+magnitude;
        f->rax=h->pos;return f;
    }
    if(op==4) {
        u64 info[3]={fz(file),h->pos,fa(file)};
        if(f->rcx>=sizeof(info) && pu(s,f->rdx,(const u8 *)info,sizeof(info)))
            f->rax=sizeof(info);
        return f;
    }
    if(op==5) {
        if((h->flags&FO_WRITE) && f->rdx<=UINT32_MAX &&
           fresize(file,(u32)f->rdx))f->rax=0;
        return f;
    }
    u64 n=f->rcx;
    if(n>UINT32_MAX)return f;
    if(op==1) {
        if(!(h->flags&FO_READ))return f;
        u64 left=h->pos<fz(file)?fz(file)-h->pos:0;
        if(n>left)n=left;
        if(!vu(s,f->rdx,n,1))return f;
        if(n) {
            const u8 *data=fd(file)+h->pos;
            int immutable=fa(file)&FIM;
            /* Namespace movement cannot invalidate the cached allocation.
             * Immutable data cannot be removed or resized. A killed task's
             * suspended continuation is discarded before handle-table reuse. */
            if(immutable){s->ir=1;__asm__ volatile("sti":::"memory");if(fxchecking)fxprobe();}
            (void)pu(s,f->rdx,data,(u32)n);
            if(immutable){__asm__ volatile("cli":::"memory");s->ir=0;}
            h->pos+=n;
        }
        f->rax=n;return f;
    }
    if(!(h->flags&FO_WRITE) || (fa(file)&FIM) || !vu(s,f->rdx,n,0))return f;
    if(!n){f->rax=0;return f;}
    u64 pos=(h->flags&FO_APPEND)?fz(file):h->pos;
    u8 *data;
    if(n>UINT32_MAX-pos || !fprepare(file,(u32)pos,(u32)n,&data))return f;
    (void)gu(s,data,f->rdx,(u32)n);
    h->pos=pos+n;f->rax=n;return f;
}
