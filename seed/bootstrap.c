#pragma GCC target("general-regs-only")
#include "bootstrap.h"
#include "build.h"
#include "files.h"
#include "heap.h"
#include "memory.h"
#include "tasks.h"
#include "interrupts.h"
#include "protocol.h"
#include "serial.h"
#include "source_snapshot.h"

/* Only task0 owns the development serial stream. User tasks may run while a
 * host response is pending; protect allocator and filesystem operations, not
 * the host's potentially long execution time. */
typedef uint8_t u8;
typedef uint16_t u16;
typedef uint32_t u32;
typedef uint64_t u64;
#define JOB_JSON_MAX 12000u
#define READ_MAX 1048576u
#define IMPORT_MAX (32u*1024u*1024u)
#define RESPONSE_MAX (FM-4u)
struct arg { const u8 *p; u32 n; };
struct wire {
    void *ctx;
    u8 (*read)(void *);
    void (*write)(void *, const u8 *, u32);
    void (*snapshot)(void *, u16);
};
static u64 lock(void) {
    u64 f;
    __asm__ volatile("pushfq; popq %0; cli":"=r"(f)::"memory");
    return f;
}
static void unlock(u64 f) {
    __asm__ volatile("pushq %0; popfq"::"r"(f):"memory","cc");
}
static void *alloc(u32 n) {
    u64 f=lock(); void *p=n?ha(n):0; unlock(f); return p;
}
static void release(void *p) {
    if(p) { u64 f=lock(); (void)hf(p); unlock(f); }
}
static u8 real_read(void *ctx) { (void)ctx; return srd(); }
static void real_write(void *ctx,const u8 *p,u32 n) {
    (void)ctx; swb(p,n);
}
static void real_snapshot(void *ctx,u16 n) { (void)ctx; sswrite(n); }
static const struct wire serial={0,real_read,real_write,real_snapshot};
static void put16(const struct wire *w,u16 n) {
    u8 b[2]={(u8)n,(u8)(n>>8)}; w->write(w->ctx,b,2);
}
static void put32(const struct wire *w,u32 n) {
    u8 b[4]={(u8)n,(u8)(n>>8),(u8)(n>>16),(u8)(n>>24)};
    w->write(w->ctx,b,4);
}
static u16 get16(const u8 *p) { return p[0]|((u16)p[1]<<8); }
static u32 get32(const u8 *p) {
    return p[0]|((u32)p[1]<<8)|((u32)p[2]<<16)|((u32)p[3]<<24);
}
static void head(const struct wire *w,u16 type,u32 id,u32 n) {
    static const u8 magic[]={'C','X','O','S'};
    w->write(w->ctx,magic,4); put16(w,1); put16(w,type);
    put32(w,id); put32(w,n);
}
static void discard(const struct wire *w,u32 n) {
    while(n--) (void)w->read(w->ctx);
}
/* The trusted host validates strict JSON and authorization. This bridge bounds
 * the UTF-8 transport; it does not reinterpret, repair or authorize requests. */
static int job_valid(const u8 *p,u32 n) {
    if(!p||!n||n>JOB_JSON_MAX||!fu(p,n)) return 0;
    for(u32 i=0;i<n;++i) if(!p[i]) return 0;
    return 1;
}
static int opaque(const u8 *p,u32 n) {
    if(!p||!n||n>255||!fu(p,n)) return 0;
    for(u32 i=0;i<n;++i) if(!p[i]) return 0;
    return 1;
}
static int decimal(const u8 *p,u32 n,u64 *v) {
    if(!p||!n||n>20||(n>1&&p[0]=='0')) return 0;
    u64 x=0;
    for(u32 i=0;i<n;++i) {
        if(p[i]<'0'||p[i]>'9'||x>(UINT64_MAX-(p[i]-'0'))/10u) return 0;
        x=x*10u+p[i]-'0';
    }
    *v=x; return 1;
}
static u32 digits(u64 n,u8 *p) {
    u8 r[20]; u32 z=0;
    do { r[z++]=(u8)('0'+n%10u); n/=10u; } while(n);
    for(u32 i=0;i<z;++i) p[i]=r[z-1u-i];
    return z;
}
static int range(const u8 *off,u32 on,const u8 *len,u32 ln,u32 *size) {
    u64 o,n;
    if(!decimal(off,on,&o)||!decimal(len,ln,&n)||n>READ_MAX||o>UINT64_MAX-n)
        return 0;
    *size=(u32)n; return 1;
}
/* Checked envelope arithmetic includes all argument lengths and optional
 * source framing. Emission can then stream without a snapshot allocation. */
static int envelope(u32 name,u16 count,const struct arg *a,
                    int snapshot,u32 sn,u32 *out) {
    if(!name||name>255||count>3||(snapshot&&count>=3)) return 0;
    u64 n=4u+(u64)name;
    for(u16 i=0;i<count;++i) n+=4u+(u64)a[i].n;
    if(snapshot) n+=4u+(u64)sn;
    if(n>FM) return 0;
    *out=(u32)n; return 1;
}
static void request(const struct wire *w,u32 id,const u8 *name,u32 nn,
                    const struct arg *a,u16 count,int snapshot,u16 sc,u32 sn) {
    u32 n;
    if(!envelope(nn,count,a,snapshot,sn,&n)) return;
    head(w,HOST_SERVICE_REQUEST,id,n);
    put16(w,(u16)nn); w->write(w->ctx,name,nn);
    put16(w,count+(snapshot?1u:0u));
    for(u16 i=0;i<count;++i) { put32(w,a[i].n); w->write(w->ctx,a[i].p,a[i].n); }
    if(snapshot) { put32(w,sn); w->snapshot(w->ctx,sc); }
}
static void job_request(const struct wire *w,u32 id,const u8 *p,u32 n,u16 sc,u32 sn) {
    static const u8 name[]="bootstrap_job";
    struct arg a={p,n};
    request(w,id,name,sizeof(name)-1u,&a,1,1,sc,sn);
}
static void artifact_request(const struct wire *w,u32 id,const struct arg *a) {
    static const u8 name[]="read_bootstrap_artifact";
    request(w,id,name,sizeof(name)-1u,a,3,0,0,0);
}
/* Invalid framing cannot safely be resynchronized on an arbitrary binary
 * stream. Match the inherited protocol's fatal handling of that case. */
static int frame_valid(const u8 *h) {
    return h[0]=='C'&&h[1]=='X'&&h[2]=='O'&&h[3]=='S'&&
           get16(h+4)==1&&get32(h+12)<=FM;
}
__attribute__((noreturn)) static void fatal(void) {
    for(;;) __asm__ volatile("pause");
}
static int response(const struct wire *w,u32 id,u32 *status,u32 *n) {
    u8 h[16],s[4];
    for(u32 i=0;i<16;++i) h[i]=w->read(w->ctx);
    if(!frame_valid(h)) fatal();
    u32 z=get32(h+12);
    if(get16(h+6)!=HOST_SERVICE_RESPONSE||get32(h+8)!=id||z<4) {
        discard(w,z); return 0;
    }
    for(u32 i=0;i<4;++i) s[i]=w->read(w->ctx);
    *status=get32(s); *n=z-4;
    if(*status>2) { discard(w,*n); return 0; }
    return 1;
}
static void fail(const struct wire *w,u32 id) {
    head(w,INVOKE_TOOL_RESPONSE,id,4); put32(w,1);
}
static void relay(const struct wire *w,u32 host,u32 tool,int exact,u32 expected) {
    u32 status,n;
    if(!response(w,host,&status,&n)) { fail(w,tool); return; }
    if(n>RESPONSE_MAX||(!status&&exact&&n!=expected)) {
        discard(w,n); fail(w,tool); return;
    }
    head(w,INVOKE_TOOL_RESPONSE,tool,4u+n); put32(w,status);
    u8 buf[256];
    while(n) {
        u32 z=n<sizeof(buf)?n:sizeof(buf);
        for(u32 i=0;i<z;++i) buf[i]=w->read(w->ctx);
        w->write(w->ctx,buf,z); n-=z;
    }
}
static int receive_exact(const struct wire *w,u32 id,u8 *p,u32 expected) {
    u32 status,n;
    if(!response(w,id,&status,&n)) return 0;
    if(status||n!=expected) { discard(w,n); return 0; }
    for(u32 i=0;i<n;++i) p[i]=w->read(w->ctx);
    return 1;
}
void tbootstrap(u32 tool,const u8 *json,u32 n) {
    if(!job_valid(json,n)) { fail(&serial,tool); return; }
    u16 count; u32 bytes;
    u64 f=lock();
    if(!ssmeasure(&count,&bytes)) { unlock(f); fail(&serial,tool); return; }
    u32 id=hostid();
    job_request(&serial,id,json,n,count,bytes);
    unlock(f);
    relay(&serial,id,tool,0,0);
}
void tbootread(u32 tool,const u8 *id,u32 in,const u8 *off,u32 on,
               const u8 *len,u32 ln) {
    u32 expected;
    if(!opaque(id,in)||!range(off,on,len,ln,&expected)) { fail(&serial,tool); return; }
    struct arg a[3]={{id,in},{off,on},{len,ln}};
    u32 host=hostid();
    artifact_request(&serial,host,a);
    relay(&serial,host,tool,1,expected);
}
static int commit(const u8 *path,u32 pn,const u8 *data,u32 n) {
    u64 f=lock();
    if(ff(path,pn)) { unlock(f); return 0; }
    int ok=fw(path,pn,0,data,n);
    /* fw can create an empty file before allocation fails. Nobody else can
     * obtain that path until this critical section ends. */
    if(!ok) (void)fr(path,pn);
    unlock(f); return ok;
}
static int import(const struct wire *w,const u8 *id,u32 in,
                  const u8 *path,u32 pn,u32 n) {
    u64 f=lock(); int exists=ff(path,pn)!=0; unlock(f);
    if(exists) return 0;
    u8 *data=alloc(n);
    if(n&&!data) return 0;
    u32 off=0; int ok=1;
    /* Even an empty artifact requires a successful host read to establish
     * existence and authorization. Staging is invisible until commit. */
    do {
        u32 z=n-off; if(z>READ_MAX) z=READ_MAX;
        u8 os[20],ls[20];
        struct arg a[3]={{id,in},{os,digits(off,os)},{ls,digits(z,ls)}};
        u32 host=hostid();
        artifact_request(w,host,a);
        if(!receive_exact(w,host,n?data+off:0,z)) { ok=0; break; }
        off+=z;
    } while(off<n);
    if(ok) ok=commit(path,pn,data,n);
    release(data); return ok;
}
void tbootimport(u32 tool,const u8 *id,u32 in,const u8 *size,u32 sn,
                 const u8 *path,u32 pn) {
    u64 n;
    if(!opaque(id,in)||!decimal(size,sn,&n)||n>IMPORT_MAX||!fpv(path,pn)||
       !import(&serial,id,in,path,pn,(u32)n)) { fail(&serial,tool); return; }
    head(&serial,INVOKE_TOOL_RESPONSE,tool,4); put32(&serial,0);
}
#include "bootstrap_tests.h"
