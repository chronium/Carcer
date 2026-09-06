#pragma GCC target("general-regs-only")
#include "tasks.h"
#include "files.h"
#include "interrupts.h"
#include "memory.h"
#include "video.h"
#include "input.h"
typedef unsigned char u8;typedef unsigned u32;typedef unsigned long u64;
#define NT 8u
#define KS (16u*1024u)
#define KC 0x08u
#define KD 0x10u
#define UD 0x1bu
#define UC 0x23u
#define UB 0x400000ull
#define ST 0x40000000ull
#define SP 16u
#define HL (ST-(SP+1u)*PG)
#define MP 511u
#define MI (MP*PG)
#define EH 16u
#define PM 0x000ffffffffff000ull
#define P 1ull
#define W 2ull
#define U 4ull
#define PF (P|W|U)
#define NX (1ull<<63)
#define XR 1u
#define XW 2u
#define XX 4u
#define XMAX 16u
#define RUN 1u
#define SLP 2u
#define ZOM 3u
#define WAI 4u
struct xh{u8 magic[4];u32 count;u64 entry,reserved;}__attribute__((packed));struct xs{u64 va,mem;u32 off,size,flags,reserved;}__attribute__((packed));
struct ts{struct tc c;u64 cr3,hb,brk,xs,wake;u32 waiter,waitfor;u8 st,ir,pr;};static struct ts slots[NT];static u8 stacks[NT][KS]__attribute__((aligned(16)));static u32 cur;static u8 ready;static volatile u8 kran;
static u64 lock(void){u64 f;__asm__ volatile("pushfq; popq %0; cli":"=r"(f)::"memory");return f;}static void unlock(u64 f){__asm__ volatile("pushq %0; popfq"::"r"(f):"memory","cc");}static u64 gc(void){u64 v;__asm__ volatile("movq %%cr3,%0":"=r"(v));return v;}static void sc(u64 v){__asm__ volatile("movq %0,%%cr3"::"r"(v):"memory");}static u64*tb(u64 p){return(u64*)mpv(p&PM);}static void cc(struct tc*d,const struct tc*s){for(u32 i=0;i<sizeof(*d)/8u;++i)((volatile u64*)d)[i]=((const volatile u64*)s)[i];}static void cz(struct tc*c){for(u32 i=0;i<sizeof(*c)/8u;++i)((u64*)c)[i]=0;c->fx[0]=0x7f;c->fx[1]=3;c->fx[24]=0x80;c->fx[25]=0x1f;}static int slot(void){for(u32 i=1;i<NT;++i)if(!slots[i].st)return(int)i;return-1;}
/* Boot regression fault injection. Only args_tests changes this budget, while
 * interrupts are disabled; normal operation always leaves it unlimited. */
static u64 task_page_budget=UINT64_MAX;
static u64 task_page(void) {
    if(task_page_budget!=UINT64_MAX) {
        if(!task_page_budget) return 0;
        --task_page_budget;
    }
    return mpa();
}
static u64*lf(u64 root,u64 a,int make){u64*t=tb(root);if(!t)return 0;for(int s=39;s>=21;s-=9){u64*e=t+((a>>s)&511u);if(!(*e&P)){if(!make)return 0;u64 p=task_page();if(!p)return 0;*e=p|PF;}if((*e&(P|U))!=(P|U)||(t=tb(*e))==0)return 0;}return t+((a>>12)&511u);}static u8*xl(u64 root,u64 a,int write){u64*e=lf(root,a,0);if(!e||!(*e&P)||!(*e&U)||(write&&!(*e&W)))return 0;u8*p=mpv(*e&PM);return p?p+(a&(PG-1u)):0;}static int em(u64*t){for(u32 i=0;i<512;++i)if(t[i]&P)return 0;return 1;}static void um(u64 root,u64 a){u64*t=tb(root),*e[3],p[3];u32 k=0;if(!t)return;for(int z=39;z>=21;z-=9){e[k]=t+((a>>z)&511u);if(!(*e[k]&P))break;p[k]=*e[k]&PM;if(!(t=tb(p[k])))break;++k;}if(k==3){u64*l=t+((a>>12)&511u);if(*l&P){u64 q=*l&PM;*l=0;if((gc()&PM)==root)__asm__ volatile("invlpg (%0)"::"r"(a):"memory");(void)mpf(q);}}while(k){int i=(int)--k;t=tb(p[i]);if(!t||!em(t))break;(void)mpf(p[i]);*e[i]=0;}}static int mm(u64 root,u64 a,u64 flags){u64*e=lf(root,a,1);if(!e){um(root,a);return 0;}if(*e&P)return 0;u64 p=task_page();if(!p){um(root,a);return 0;}*e=p|flags;if((gc()&PM)==root)__asm__ volatile("invlpg (%0)"::"r"(a):"memory");return 1;}static void tr(u64 p,u32 level){u64*t=tb(p);if(t)for(u32 i=0;i<512;++i)if(t[i]&P){u64 q=t[i]&PM;if(level)tr(q,level-1u);else(void)mpf(q);}(void)mpf(p);}static void sf(u64 root){u64*r=tb(root);if(r&&(r[0]&P))tr(r[0]&PM,2);(void)mpf(root);}
static int vu(struct ts*s,u64 a,u64 n,int write){if(!n)return 1;if(a>=ST||n>ST-a)return 0;u64 end=a+n;while(a<end){if(!xl(s->cr3,a,write))return 0;u64 q=PG-(a&(PG-1u));if(q>end-a)q=end-a;a+=q;}return 1;}static int gu(struct ts*s,u8*d,u64 a,u32 n){if(!vu(s,a,n,0))return 0;while(n){u8*p=xl(s->cr3,a,0);u32 q=PG-(u32)(a&(PG-1u));if(q>n)q=n;for(u32 i=0;i<q;++i)d[i]=p[i];d+=q;a+=q;n-=q;}return 1;}static int pu(struct ts*s,u64 a,const u8*d,u32 n){if(!vu(s,a,n,1))return 0;while(n){u8*p=xl(s->cr3,a,1);u32 q=PG-(u32)(a&(PG-1u));if(q>n)q=n;for(u32 i=0;i<q;++i)p[i]=d[i];d+=q;a+=q;n-=q;}return 1;}
static u64 nr(void){u64 root=task_page();if(!root)return 0;u64*r=tb(root),*k=tb(slots[0].cr3);if(!r||!k){(void)mpf(root);return 0;}for(u32 i=0;i<512;++i)r[i]=k[i];r[0]=0;return root;}static int as(u64 root){for(u32 i=0;i<SP;++i)if(!mm(root,ST-(u64)(i+1u)*PG,PF|NX))return 0;return 1;}static int ms(const u8*image,u32 size,u64*out,u64*heap){u64 root=nr();if(!root)return 0;u32 pages=(size+PG-1u)/PG;for(u32 i=0;i<pages;++i){u64 a=UB+(u64)i*PG;if(!mm(root,a,PF)){sf(root);return 0;}u8*d=xl(root,a,1);u32 n=size-i*PG;if(n>PG)n=PG;for(u32 j=0;j<n;++j)d[j]=image[i*PG+j];}if(!as(root)){sf(root);return 0;}*heap=UB+(u64)pages*PG;*out=root;return 1;}static int m2(const u8*d,u32 n,u64*out,u64*heap,u64*entry){if(n<sizeof(struct xh))return 0;const struct xh*h=(const void*)d;if(h->magic[0]!='C'||h->magic[1]!='X'||h->magic[2]!='E'||h->magic[3]!='2'||!h->count||h->count>XMAX||h->reserved)return 0;u64 head=sizeof(*h)+(u64)h->count*sizeof(struct xs),pages=0,top=UB;int ent=0;if(head>n)return 0;const struct xs*v=(const void*)(d+sizeof(*h));for(u32 i=0;i<h->count;++i){const struct xs*x=v+i;if(!x->mem||(x->va&(PG-1u))||(x->mem&(PG-1u))||x->va<UB||x->va>=HL||x->mem>HL-x->va||x->size>x->mem||x->off<head||x->off>n||x->size>n-x->off||x->reserved||(x->flags&~7u)||!(x->flags&XR)||((x->flags&(XW|XX))==(XW|XX)))return 0;u64 end=x->va+x->mem;pages+=x->mem/PG;if(pages>16384u)return 0;if(end>top)top=end;if((x->flags&XX)&&h->entry>=x->va&&h->entry<x->va+x->size)ent=1;for(u32 j=0;j<i;++j)if(x->va<v[j].va+v[j].mem&&v[j].va<end)return 0;}if(!ent)return 0;u64 root=nr();if(!root)return 0;for(u32 i=0;i<h->count;++i){const struct xs*x=v+i;u64 fl=P|U|((x->flags&XW)?W:0)|((x->flags&XX)?0:NX);for(u64 a=0;a<x->mem;a+=PG)if(!mm(root,x->va+a,fl)){sf(root);return 0;}for(u32 j=0;j<x->size;){u8*q=xl(root,x->va+j,0);u32 z=PG-(u32)((x->va+j)&(PG-1u));if(z>x->size-j)z=x->size-j;for(u32 k=0;k<z;++k)q[k]=d[x->off+j+k];j+=z;}}if(!as(root)){sf(root);return 0;}*out=root;*heap=top;*entry=h->entry;return 1;}static int sb(struct ts*s,u64 want){if((want&(PG-1u))||want<s->hb||want>HL)return 0;u64 old=s->brk,a=old;if(want>old){for(;a<want;a+=PG)if(!mm(s->cr3,a,PF|NX)){while(a>old){a-=PG;um(s->cr3,a);}return 0;}}else for(a=old;a>want;){a-=PG;um(s->cr3,a);}s->brk=want;return 1;}
static int nxok(void){u32 a,b,c,d,l,h;a=0x80000000u;__asm__ volatile("cpuid":"=a"(a),"=b"(b),"=c"(c),"=d"(d):"a"(a));if(a<0x80000001u)return 0;a=0x80000001u;__asm__ volatile("cpuid":"=a"(a),"=b"(b),"=c"(c),"=d"(d):"a"(a));if(!(d&(1u<<20)))return 0;__asm__ volatile("rdmsr":"=a"(l),"=d"(h):"c"(0xc0000080u));l|=1u<<11;__asm__ volatile("wrmsr"::"a"(l),"d"(h),"c"(0xc0000080u));u64 q;__asm__ volatile("mov %%cr0,%0":"=r"(q));q|=1ull<<16;__asm__ volatile("mov %0,%%cr0"::"r"(q):"memory");return 1;}
__attribute__((used))static void td(void){slots[cur].st=0;}static void kfn(void){kran=1;}__attribute__((naked,noreturn,used))static void rt(void){__asm__ volatile("cli\ncall td\nsti\n1: hlt\njmp 1b\n");}int tnew(void(*entry)(void)){if(!ready||!entry)return-1;u64 f=lock();int id=slot();if(id<0){unlock(f);return-1;}struct ts*s=&slots[id];cz(&s->c);uintptr_t stack=(uintptr_t)(stacks[id]+KS);stack=(stack&~(uintptr_t)15u)-8u;*(u64*)stack=(u64)(uintptr_t)rt;s->c.rip=(u64)(uintptr_t)entry;s->c.cs=KC;s->c.rflags=0x202;s->c.rsp=stack;s->c.ss=KD;s->cr3=slots[0].cr3;s->hb=s->brk=s->xs=s->wake=0;s->ir=s->pr=0;s->waiter=s->waitfor=0;s->st=RUN;unlock(f);return id;}static int la(int id,u64 root,u64 heap,u64 rip){struct ts*s=&slots[id];cz(&s->c);s->c.rip=rip;s->c.cs=UC;s->c.rflags=0x202;s->c.rsp=ST;s->c.ss=UD;s->cr3=root;s->hb=s->brk=heap;s->xs=s->wake=0;s->ir=s->pr=0;s->waiter=s->waitfor=0;s->st=RUN;return id;}int tuser(const u8*image,u32 size,u32 entry){if(!ready||!image||!size||size>MI||entry>=size)return-1;u64 f=lock();int id=slot();u64 root,heap;if(id<0||!ms(image,size,&root,&heap)){unlock(f);return-1;}la(id,root,heap,UB+entry);unlock(f);return id;}static int t2(const u8*d,u32 n){u64 f=lock(),root,heap,entry;int id=slot();if(id<0||!m2(d,n,&root,&heap,&entry)){unlock(f);return-1;}la(id,root,heap,entry);unlock(f);return id;}static u32 r32(const u8*p){return(u32)p[0]|((u32)p[1]<<8)|((u32)p[2]<<16)|((u32)p[3]<<24);}static int loadfile(const u8*p,u32 n){if(!ready||!p||!n)return-1;struct file*f=ff(p,n);if(!f||fz(f)<EH)return-1;const u8*d=fd(f);u32 z=fz(f),size=r32(d+4),entry=r32(d+8);if(d[0]=='C'&&d[1]=='X'&&d[2]=='E'&&d[3]=='2')return t2(d,z);if(d[0]!='C'||d[1]!='X'||d[2]!='E'||d[3]!='1'||r32(d+12)||size!=z-EH)return-1;return tuser(d+EH,size,entry);}
/* Callers hold the single scheduling CPU's interrupt lock across file lookup,
 * image copy and publication. No child can execute before argument setup. */
int tfile(const u8 *p,u32 n) {
    u64 flags=lock();
    int id=loadfile(p,n);
    unlock(flags);
    return id;
}
#define ARG_COUNT 32u
#define ARG_BYTES 4096u
struct argpack { u32 count,used,offset[ARG_COUNT]; u8 data[ARG_BYTES]; };
struct argspan { u64 address,length; };
_Static_assert(sizeof(struct argspan)==16,"argument span layout");

/* Snapshot all parent data before allocating or publishing a child. Argument
 * bytes need not be UTF-8, but embedded NULs cannot form a C argument string. */
static int getargs(struct ts *parent,u64 vector,u64 count,struct argpack *a) {
    a->count=a->used=0;
    if(count>ARG_COUNT || !vu(parent,vector,count*sizeof(struct argspan),0))
        return 0;
    for(u32 i=0;i<(u32)count;++i) {
        struct argspan span;
        if(!gu(parent,(u8 *)&span,vector+(u64)i*sizeof(span),sizeof(span)) ||
           span.length>=ARG_BYTES-a->used)
            return 0;
        a->offset[i]=a->used;
        if(!gu(parent,a->data+a->used,span.address,(u32)span.length))
            return 0;
        for(u32 j=0;j<(u32)span.length;++j)
            if(!a->data[a->used+j]) return 0;
        a->used+=(u32)span.length;
        a->data[a->used++]=0;
    }
    a->count=(u32)count;
    return 1;
}
static int launchargs(const u8 *path,u32 length,const struct argpack *a) {
    int id=loadfile(path,length);
    if(id<0) return -1;
    struct ts *child=&slots[id];
    u64 strings=ST-a->used;
    u64 vector=(strings-(a->count+1u)*sizeof(u64))&~15ull;
    u64 pointers[ARG_COUNT+1u];
    for(u32 i=0;i<a->count;++i) pointers[i]=strings+a->offset[i];
    pointers[a->count]=0;
    if(!pu(child,strings,a->data,a->used) ||
       !pu(child,vector,(const u8 *)pointers,(a->count+1u)*sizeof(u64))) {
        (void)tkill((u32)id);
        return -1;
    }
    child->c.rdi=a->count;
    child->c.rsi=vector;
    child->c.rsp=vector;
    return id;
}

/* All wait graph changes and result delivery run with interrupts disabled.
 * A reservation prevents ordinary reap from consuming a blocked wait's target.
 * Blocked tasks own private address spaces, so validated output stays mapped. */
static int active(const struct ts *s) {
    return s->st == RUN || s->st == SLP || s->st == WAI;
}
static void detach_wait(u32 id) {
    struct ts *s = &slots[id];
    u32 target = s->waitfor;
    if (target && target < NT && slots[target].waiter == id)
        slots[target].waiter = 0;
    s->waitfor = 0;
}
static void deliver_wait(u32 target, int completed) {
    struct ts *s = &slots[target];
    u32 id = s->waiter;
    s->waiter = 0;
    if (!id || id >= NT) return;
    struct ts *w = &slots[id];
    if (w->st != WAI || w->waitfor != target) return;
    int copied = completed && pu(w, w->c.rsi, (const u8 *)&s->xs, 8);
    w->c.rax = copied ? 1 : UINT64_MAX;
    w->waitfor = 0;
    w->st = RUN;
    if (copied) s->st = 0;
}
int tkill(u32 id) {
    u64 flags = lock();
    int ok = ready && id && id < NT && id != cur && active(&slots[id]);
    if (ok) {
        struct ts *s = &slots[id];
        detach_wait(id);
        s->st = s->ir = s->pr = 0;
        if (s->cr3 != slots[0].cr3) sf(s->cr3);
        s->cr3 = 0;
        deliver_wait(id, 0);
    }
    unlock(flags);
    return ok;
}
int twait(u32 id, u64 *status) {
    u64 flags = lock();
    int state = -1;
    if (ready && status && id && id < NT) {
        struct ts *s = &slots[id];
        if (!s->waiter) {
            if (active(s)) state = 0;
            else if (s->st == ZOM && !s->cr3) {
                *status = s->xs;
                s->st = 0;
                state = 1;
            }
        }
    }
    unlock(flags);
    return state;
}
static struct tc *block_wait(struct tc *f) {
    struct ts *w = &slots[cur];
    u64 target = f->rdi;
    if (!target || target >= NT || target == cur ||
        !vu(w, f->rsi, 8, 1)) goto fail;
    struct ts *s = &slots[target];
    if (s->waiter) goto fail;
    if (s->st == ZOM && !s->cr3) {
        if (!pu(w, f->rsi, (const u8 *)&s->xs, 8)) goto fail;
        s->st = 0;
        f->rax = 1;
        return f;
    }
    if (!active(s) || s->cr3 == slots[0].cr3) goto fail;
    /* Existing waits form a directed acyclic graph. Reject a new cycle,
     * and bound the traversal defensively even if that invariant breaks. */
    u32 at = (u32)target;
    for (u32 depth = 0; ; ++depth) {
        if (at == cur || depth >= NT) goto fail;
        if (slots[at].st != WAI) break;
        at = slots[at].waitfor;
        if (!at || at >= NT) goto fail;
    }
    s->waiter = cur;
    w->waitfor = (u32)target;
    cc(&w->c, f);
    w->st = WAI;
    return tsched(f, 0);
fail:
    f->rax = UINT64_MAX;
    return f;
}
struct tc*tsched(struct tc*frame,int tick){if(!ready)return frame;if(tick){u64 now=tnow();for(u32 i=1;i<NT;++i){if(slots[i].st==SLP&&slots[i].wake<=now)slots[i].st=RUN;if((frame->cs&3u)==3u&&slots[i].st==RUN&&slots[i].ir&&i!=cur)slots[i].pr=1;}}u32 pv=cur;if(slots[pv].st==RUN)cc(&slots[pv].c,frame);u32 next=pv;for(u32 d=1;d<=NT;++d){u32 i=(pv+d)%NT;if(slots[i].st==RUN){next=i;break;}}cur=next;itstack(stacks[next]+KS);if(slots[next].cr3!=gc())sc(slots[next].cr3);if(pv!=next&&!active(&slots[pv])&&slots[pv].cr3){if(slots[pv].st==ZOM)sf(slots[pv].cr3);slots[pv].cr3=0;if(slots[pv].st==ZOM)deliver_wait(pv,1);}return&slots[next].c;}
static volatile u32 fxchecking,fxprobes,fxbad;
static void fxenv(void){u8 b[512]__attribute__((aligned(16)));__asm__ volatile("fxsave64 %0":"=m"(b));if(*(uint16_t*)b!=0x37f||*(uint16_t*)(b+2)||*(u32*)(b+24)!=0x1f80)fxbad=1;}
__attribute__((naked,noinline,used))static void fxprobe(void){__asm__ volatile("mov $0x12345678,%ecx\nmovq %rcx,%xmm0\nmov $0x76543210,%edx\nmovq %rdx,%xmm15\nmov ticks(%rip),%rax\nadd $3,%rax\n1: cmp %rax,ticks(%rip)\njae 2f\npause\njmp 1b\n2: movq %xmm0,%rax\ncmp %rcx,%rax\njne 3f\nmovq %xmm15,%rax\ncmp %rdx,%rax\nje 4f\n3: movl $1,fxbad(%rip)\n4: incl fxprobes(%rip)\nret\n");}
static int gp(struct ts*s,struct tc*f,u8*p){return f->rsi&&f->rsi<=FPL&&gu(s,p,f->rdi,(u32)f->rsi)&&fpv(p,(u32)f->rsi);}static struct tc *spawnargs(struct tc *f) {
    u8 path[FPL];
    struct argpack args;
    if(!gp(&slots[cur],f,path) ||
       !getargs(&slots[cur],f->rdx,f->rcx,&args)) {
        f->rax=UINT64_MAX;
        return f;
    }
    int id=launchargs(path,(u32)f->rsi,&args);
    f->rax=id<0?UINT64_MAX:(u64)id;
    return f;
}
static struct tc *filectl(struct tc *f) {
    struct ts *s=&slots[cur];
    u8 path[FPL],dest[FPL];
    int ok=0;
    if(!gp(s,f,path)) goto done;
    switch(f->rdx) {
    case 0:
        if(!f->rcx && !f->r8) ok=fcreate(path,(u32)f->rsi);
        break;
    case 1:
        if(f->rcx<=UINT32_MAX && !f->r8)
            ok=ft(path,(u32)f->rsi,(u32)f->rcx);
        break;
    case 2:
        if(!f->rcx && !f->r8) ok=fr(path,(u32)f->rsi);
        break;
    case 3:
        if(f->r8 && f->r8<=FPL &&
           gu(s,dest,f->rcx,(u32)f->r8) && fpv(dest,(u32)f->r8))
            ok=fmove(path,(u32)f->rsi,dest,(u32)f->r8);
        break;
    }
done:
    f->rax=ok?0:UINT64_MAX;
    return f;
}
static struct tc *readkeys(struct tc *f) {
    struct ts *s=&slots[cur];
    if(!f->rsi){f->rax=(u64)key_available();return f;}
    struct key_event events[KEY_BATCH];
    u64 cursor,after,bytes=f->rsi*sizeof(struct key_event);
    f->rax=UINT64_MAX;
    if(!key_available() || f->rsi>KEY_BATCH ||
       !vu(s,f->rdx,8,1) || !gu(s,(u8 *)&cursor,f->rdx,8) ||
       !vu(s,f->rdi,bytes,1)) return f;
    /* The cursor must not alias the output records. Ranges are bounded by ST. */
    if(f->rdx<f->rdi+bytes && f->rdi<f->rdx+8) return f;
    int n=key_read(cursor,events,(u32)f->rsi,&after);
    if(n<0)return f;
    (void)pu(s,f->rdi,(const u8 *)events,(u32)n*sizeof(struct key_event));
    (void)pu(s,f->rdx,(const u8 *)&after,8);
    f->rax=(u64)n;return f;
}
/* Namespace snapshot: one interrupt-disabled call, bounded by FMC records.
 * Prevalidate the complete written span before publishing any record. */
struct file_record {u32 size,attributes;uint16_t length,reserved;u32 reserved2;u8 path[256];};
_Static_assert(sizeof(struct file_record)==272,"file record layout");
static struct tc *filelist(struct tc *f) {
    struct ts *s=&slots[cur];
    u64 capacity=f->rsi;
    if(!capacity){f->rax=fc;return f;}
    f->rax=UINT64_MAX;
    if(capacity>FMC || capacity<fc ||
       !vu(s,f->rdi,(u64)fc*sizeof(struct file_record),1))return f;
    for(u32 i=0;i<fc;i++) {
        struct file_record r;
        u8 *bytes=(u8 *)&r;
        for(u32 j=0;j<sizeof(r);j++)bytes[j]=0;
        r.size=fz(&files[i]);r.attributes=fa(&files[i]);r.length=files[i].n;
        for(u32 j=0;j<r.length;j++)r.path[j]=files[i].p[j];
        (void)pu(s,f->rdi+(u64)i*sizeof(r),bytes,sizeof(r));
    }
    f->rax=fc;return f;
}
struct tc*tsys(struct tc*f){if(!ready||!f||!cur||!slots[cur].cr3){if(f)f->rax=UINT64_MAX;return f;}struct ts*s=&slots[cur];if(fxchecking)fxenv();if(f->rax==0){s->xs=f->rdi;s->st=ZOM;return tsched(f,0);}if(f->rax==12)return block_wait(f);if(f->rax==13)return spawnargs(f);if(f->rax==14)return filectl(f);if(f->rax==15)return readkeys(f);if(f->rax==16)return filelist(f);if(f->rax==6){u64 status;int state=f->rdi<=UINT32_MAX&&vu(s,f->rsi,8,1)?twait((u32)f->rdi,&status):-1;if(state==1)(void)pu(s,f->rsi,(const u8*)&status,8);f->rax=state<0?UINT64_MAX:(u64)state;return f;}if(f->rax==7){if(!f->rdi)f->rax=s->brk;else f->rax=sb(s,f->rdi)?s->brk:UINT64_MAX;return f;}if(f->rax==8){f->rax=tnow();return f;}if(f->rax==11){u64 now=tnow();if(!f->rdi){f->rax=0;return f;}if(f->rdi>UINT64_MAX-now){f->rax=UINT64_MAX;return f;}f->rax=0;cc(&s->c,f);s->wake=now+f->rdi;s->st=SLP;return tsched(f,0);}if(f->rax==9){struct vinfo v;vinfo(&v);f->rax=f->rsi>=sizeof(v)&&pu(s,f->rdi,(const u8*)&v,sizeof(v))?sizeof(v):UINT64_MAX;return f;}if(f->rax==10){if(f->rdx>UINT32_MAX||f->rcx>UINT32_MAX||f->r8>UINT32_MAX||f->r9>UINT32_MAX){f->rax=UINT64_MAX;return f;}u32 pitch,z;u8*d=vtarget((u32)f->rdx,(u32)f->rcx,(u32)f->r8,(u32)f->r9,&pitch);if(!d||(z=(u32)f->r8*4u)>f->rsi){f->rax=UINT64_MAX;return f;}u64 a=f->rdi;for(u32 i=0;i<(u32)f->r9;++i){if(!vu(s,a,z,0)||(i+1u<(u32)f->r9&&f->rsi>UINT64_MAX-a)){f->rax=UINT64_MAX;return f;}a+=f->rsi;}a=f->rdi;for(u32 i=0;i<(u32)f->r9;++i){(void)gu(s,d+(u64)i*pitch,a,z);a+=f->rsi;}f->rax=0;return f;}if(f->rax<1||f->rax>5){f->rax=UINT64_MAX;return f;}u8 path[FPL];if(!gp(s,f,path)){f->rax=UINT64_MAX;return f;}if(f->rax==4){u64 z=f->r8;u8*d;if(f->rdx>UINT32_MAX||z>UINT32_MAX||(z&&!vu(s,f->rcx,z,0))||!fws(path,(u32)f->rsi,(u32)f->rdx,(u32)z,&d))f->rax=UINT64_MAX;else{if(z)(void)gu(s,d,f->rcx,(u32)z);f->rax=z;}return f;}if(f->rax==5){int id=tfile(path,(u32)f->rsi);f->rax=id<0?UINT64_MAX:(u64)id;return f;}struct file*file=ff(path,(u32)f->rsi);if(!file){f->rax=UINT64_MAX;return f;}if(f->rax==1){f->rax=fz(file);return f;}if(f->rax==3){f->rax=fa(file);return f;}u32 size=fz(file);const u8*content=fd(file);int immutable=fa(file)&FIM;if(f->rdx>size){f->rax=UINT64_MAX;return f;}u64 left=size-(u32)f->rdx,count=f->r8<left?f->r8:left;if(count>UINT32_MAX||!vu(s,f->rcx,count,1)){f->rax=UINT64_MAX;return f;}if(count){if(immutable){s->ir=1;__asm__ volatile("sti":::"memory");if(fxchecking)fxprobe();}(void)pu(s,f->rcx,content+(u32)f->rdx,(u32)count);if(immutable){__asm__ volatile("cli":::"memory");s->ir=0;}}f->rax=count;return f;}
static const u8 workup[]={0x48,0xb8,0,1,0x40,0,0,0,0,0,0x48,0xff,0,0xeb,0xfb},workdown[]={0x48,0xb8,0,1,0x40,0,0,0,0,0,0x48,0xff,8,0xeb,0xfb};static const u8 workhead[]={'C','X','E','1',0,0x20,0,0,0x80,0x10,0,0,0,0,0},cx[]={'C','X','E','1',9,0,0,0,0,0,0,0,0,0,0,0,0xbf,37,0,0,0,0x31,0xc0,0xcd,0x80};extern const u8 abiend[];__attribute__((naked,noinline,used))static void abitest(void){__asm__ volatile("mov $99,%eax\nint $0x80\ncmp $-1,%rax\njne 9f\n"
"mov $1,%eax\nmov $0x400ff8,%edi\nmov $16,%esi\nint $0x80\ncmp $0x2010,%rax\njne 9f\n"
"mov $2,%eax\nmov $0x401040,%edi\nmov $16,%esi\nxor %edx,%edx\nmov $0x400ff8,%ecx\nmov $16,%r8d\nint $0x80\ncmp $16,%rax\njne 9f\ncmpl $0x31455843,0x400ff8\njne 9f\ncmpl $0x2000,0x400ffc\njne 9f\n"
"movl $0x76543210,0x401ffc\nmov $2,%eax\nmov $0x401040,%edi\nmov $16,%esi\nxor %edx,%edx\nmov $0x401ffc,%ecx\nmov $8,%r8d\nint $0x80\ncmp $-1,%rax\njne 9f\ncmpl $0x76543210,0x401ffc\njne 9f\n"
"mov $7,%eax\nxor %edi,%edi\nint $0x80\ncmp $0x402000,%rax\njne 9f\nmov $7,%eax\nmov $0x402001,%edi\nint $0x80\ncmp $-1,%rax\njne 9f\nmov $7,%eax\nxor %edi,%edi\nint $0x80\ncmp $0x402000,%rax\njne 9f\nmov $7,%eax\nmov $0x403000,%edi\nint $0x80\ncmp $0x403000,%rax\njne 9f\n"
"movb $0xff,0x401070\nmov $1,%eax\nmov $0x401070,%edi\nmov $9,%esi\nint $0x80\ncmp $-1,%rax\njne 9f\nmovb $0x74,0x401070\n"
"mov $4,%eax\nmov $0x401070,%edi\nmov $9,%esi\nxor %edx,%edx\nmov $0x401040,%ecx\nmov $1,%r8d\nint $0x80\ncmp $1,%rax\njne 9f\n"
"mov $7,%eax\nmov $0x404000,%edi\nint $0x80\ncmp $0x404000,%rax\njne 9f\nmov $2,%eax\nmov $0x401040,%edi\nmov $16,%esi\nxor %edx,%edx\nmov $0x402ff8,%ecx\nmov $16,%r8d\nint $0x80\ncmp $16,%rax\njne 9f\ncmpl $0x31455843,0x402ff8\njne 9f\n"
"mov $9,%eax\nmov $1,%edi\nmov $32,%esi\nint $0x80\ncmp $-1,%rax\njne 9f\nmov $9,%eax\nmov $0x403020,%edi\nint $0x80\ncmp $32,%rax\njne 9f\ncmpl $32,0x403020\njne 9f\ncmpl $1,0x403030\njne 9f\nmovl $0x112233,0x402ffc\nmovl $0x445566,0x403000\nmov $10,%eax\nmov $0x402ffc,%edi\nmov $8,%esi\nmov 0x403024,%edx\nxor %ecx,%ecx\nmov $2,%r8d\nmov $1,%r9d\nint $0x80\ncmp $-1,%rax\njne 9f\nmov $10,%eax\nxor %edx,%edx\nint $0x80\ntest %rax,%rax\njne 9f\nmovl $0xaabbccdd,0x3ffffff8\nmovl $0x66778899,0x3ffffffc\nmov $0x3ffffff8,%edi\nmov $2,%r9d\nmov $10,%eax\nint $0x80\ncmp $-1,%rax\njne 9f\n"
"mov $7,%eax\nmov $0x403000,%edi\nint $0x80\ncmp $0x403000,%rax\njne 9f\nmov $2,%eax\nmov $0x401040,%edi\nmov $16,%esi\nxor %edx,%edx\nmov $0x403000,%ecx\nmov $1,%r8d\nint $0x80\ncmp $-1,%rax\njne 9f\nmov $7,%eax\nmov $0x404000,%edi\nint $0x80\ncmpb $0,0x403000\njne 9f\nmov $7,%eax\nmov $0x2404000,%edi\nint $0x80\ncmp $0x2404000,%rax\njne 9f\nmov $16,%ebp\n6: mov $2,%eax\nmov $0x401060,%edi\nmov $14,%esi\nxor %edx,%edx\nmov $0x404000,%ecx\nmov $0x2000000,%r8d\nint $0x80\ncmp $0x2000000,%rax\njne 9f\ndec %ebp\njnz 6b\nmovl $0x12345678,0x600000\nmov $7,%eax\nmov $0x404000,%edi\nint $0x80\ncmp $0x404000,%rax\njne 9f\nmov $2,%eax\nmov $0x401040,%edi\nmov $16,%esi\nxor %edx,%edx\nmov $0x600000,%ecx\nmov $1,%r8d\nint $0x80\ncmp $-1,%rax\njne 9f\n"
"mov $2,%eax\nmov $0x401040,%edi\nmov $16,%esi\nxor %edx,%edx\nmov $0x3ffffff8,%ecx\nmov $8,%r8d\nint $0x80\ncmp $8,%rax\njne 9f\ncmpl $0x31455843,0x3ffffff8\njne 9f\n"
"mov $3,%eax\nmov $0x401060,%edi\nmov $14,%esi\nint $0x80\ncmp $1,%rax\njne 9f\nmov $4,%eax\nmov $0x401060,%edi\nmov $14,%esi\nxor %edx,%edx\nmov $0x400ff8,%ecx\nmov $1,%r8d\nint $0x80\ncmp $-1,%rax\njne 9f\nmov $4,%eax\nmov $0x401060,%edi\nmov $14,%esi\nxor %edx,%edx\nxor %ecx,%ecx\nxor %r8d,%r8d\nint $0x80\ncmp $-1,%rax\njne 9f\n"
"mov $1,%eax\nmov $0x401070,%edi\nmov $9,%esi\nint $0x80\ncmp $1,%rax\njne 9f\nmov $2,%eax\nxor %edx,%edx\nmov $0x3ffffff0,%ecx\nmov $1,%r8d\nint $0x80\ncmp $1,%rax\njne 9f\ncmpb $0x62,0x3ffffff0\njne 9f\n"
"mov $4,%eax\nmov $0x401040,%edi\nmov $16,%esi\nmov $0x1090,%edx\nmov $0x402ff8,%ecx\nmov $16,%r8d\nint $0x80\ncmp $16,%rax\njne 9f\nmov $2,%eax\nmov $0x401040,%edi\nmov $16,%esi\nmov $0x1090,%edx\nmov $0x3fffffe0,%ecx\nmov $16,%r8d\nint $0x80\ncmp $16,%rax\njne 9f\ncmpl $0x31455843,0x3fffffe0\njne 9f\n"
"mov $5,%eax\nmov $0x401010,%edi\nmov $13,%esi\nint $0x80\ncmp $-1,%rax\nje 9f\nmov %eax,%ebx\n7: mov $6,%eax\nmov %ebx,%edi\nmov $0x3fffffd8,%esi\nint $0x80\ntest %rax,%rax\nje 7b\ncmp $1,%rax\njne 9f\ncmpq $37,0x3fffffd8\njne 9f\nmov $6,%eax\nint $0x80\ncmp $-1,%rax\njne 9f\nmov $42,%edi\njmp 8f\n9: mov $13,%edi\n8: xor %eax,%eax\nint $0x80\nud2\n.global abiend\nabiend:\n");}
static int imt(void){static const u8 p[]="test/immutable",d[]="sealed";u8 x='X';if(!fw(p,sizeof(p)-1,0,d,sizeof(d)-1)||!ft(p,sizeof(p)-1,0x2000000)||!fsl(p,sizeof(p)-1))return 0;struct file*f=ff(p,sizeof(p)-1);if(!f||fa(f)!=FIM||fz(f)!=0x2000000)return 0;if(fw(p,sizeof(p)-1,0,&x,1)||fw(p,sizeof(p)-1,0,0,0)||ft(p,sizeof(p)-1,sizeof(d)-1)||fr(p,sizeof(p)-1))return 0;return ff(p,sizeof(p)-1)!=0;}static int dn(u32 id,u64 status){u64 f=lock();int ok=id<NT&&slots[id].st==ZOM&&slots[id].xs==status&&!slots[id].cr3;if(ok)slots[id].st=0;unlock(f);return ok;}static u32 xi(u8*z,const u8*c,u32 n){for(u32 i=0;i<160;++i)z[i]=0;struct xh*h=(void*)z;h->magic[0]='C';h->magic[1]='X';h->magic[2]='E';h->magic[3]='2';h->count=2;h->entry=UB;struct xs*v=(void*)(z+sizeof(*h));v[0].va=UB;v[0].mem=PG;v[0].off=sizeof(*h)+2u*sizeof(*v);v[0].size=n;v[0].flags=XR|XX;v[1].va=UB+PG;v[1].mem=PG;v[1].off=v[0].off+n;v[1].size=1;v[1].flags=XR|XW;for(u32 i=0;i<n;++i)z[v[0].off+i]=c[i];z[v[1].off]=0x7b;return v[1].off+1u;}static int aw(u32 id,u64 status){u64 t=tnow();while(tnow()-t<THZ)if(dn(id,status))return 1;return 0;}static int x2(void){static const u8 good[]={0xb8,8,0,0,0,0xcd,0x80,0x48,0x85,0xc0,0x74,0x20,0x48,0xb8,0,0x10,0x40,0,0,0,0,0,0x80,0x38,0x7b,0x75,0x11,0x48,0xff,0xc0,0x80,0x38,0,0x75,9,0xbf,42,0,0,0,0x31,0xc0,0xcd,0x80,0x0f,0x0b},wr[]={0xc6,4,0x25,0,0,0x40,0,0},jump[]={0x48,0xb8,0,0x10,0x40,0,0,0,0,0,0xff,0xe0},spin[]={0x48,0xb8,0,0x10,0x40,0,0,0,0,0,0x48,0xff,0,0xeb,0xfb};u8 z[160];u32 n=xi(z,good,sizeof(good));u64 free=mfc();struct xs*v=(void*)(z+sizeof(struct xh));v[0].flags=XR|XW|XX;if(t2(z,n)>=0)return 0;v[0].flags=XR|XX;v[1].va=UB+1;if(t2(z,n)>=0)return 0;v[1].va=UB;if(t2(z,n)>=0)return 0;v[1].va=UB+PG;v[1].size=PG+1u;if(t2(z,n)>=0)return 0;v[1].size=1;if(mfc()!=free)return 0;int id=t2(z,n);if(id<0||!aw((u32)id,42))return 0;n=xi(z,wr,sizeof(wr));id=t2(z,n);if(id<0||!aw((u32)id,UINT64_MAX))return 0;n=xi(z,jump,sizeof(jump));id=t2(z,n);if(id<0||!aw((u32)id,UINT64_MAX))return 0;n=xi(z,spin,sizeof(spin));int a=t2(z,n);z[sizeof(struct xh)+2u*sizeof(struct xs)+12u]=8;int b=t2(z,n);if(a<0||b<0)return 0;volatile u64*p=(volatile u64*)xl(slots[a].cr3,UB+PG,1),*q=(volatile u64*)xl(slots[b].cr3,UB+PG,1);u64 t=tnow();while((*p==0x7b||*q==0x7b)&&tnow()-t<THZ)__asm__ volatile("pause");int ok=*p!=0x7b&&*q!=0x7b&&tkill((u32)a)&&tkill((u32)b);return ok&&mfc()==free;}static int slt(void){static const u8 c[]={184,11,0,0,0,49,255,205,128,72,133,192,117,70,184,11,0,0,0,72,199,199,255,255,255,255,205,128,72,131,248,255,117,50,184,8,0,0,0,205,128,72,137,195,184,11,0,0,0,191,3,0,0,0,205,128,72,133,192,117,23,184,8,0,0,0,205,128,72,41,216,72,131,248,3,114,7,191,42,0,0,0,235,5,191,13,0,0,0,49,192,205,128,15,11};u8 z[256];u32 n=xi(z,c,sizeof(c));u64 free=mfc();int a=t2(z,n),b=tuser(workup,sizeof(workup),0);if(a<0||b<0)return 0;u64 t=tnow(),status;while(slots[a].st!=SLP&&tnow()-t<THZ)__asm__ volatile("pause");volatile u64*q=(volatile u64*)xl(slots[b].cr3,UB+0x100,1);if(slots[a].st!=SLP||!q||twait((u32)a,&status))return 0;u64 before=*q,wake=slots[a].wake;t=tnow();while(slots[a].st==SLP&&tnow()-t<THZ)__asm__ volatile("pause");if(tnow()<wake||*q==before||!aw((u32)a,42)||!tkill((u32)b)||mfc()!=free)return 0;n=xi(z,c,sizeof(c));struct xs*v=(void*)(z+sizeof(struct xh));z[v[0].off+50u]=100;free=mfc();a=t2(z,n);if(a<0)return 0;t=tnow();while(slots[a].st!=SLP&&tnow()-t<THZ)__asm__ volatile("pause");return slots[a].st==SLP&&!twait((u32)a,&status)&&tkill((u32)a)&&twait((u32)a,&status)<0&&mfc()==free;}extern const u8 fxend[];__attribute__((naked,noinline,used))static void fxwork(void){__asm__ volatile("fxsave64 0x401100\ncmpw $0x37f,0x401100\njne 9f\ncmpw $0,0x401102\njne 9f\ncmpb $0,0x401104\njne 9f\ncmpl $0x1f80,0x401118\njne 9f\nmov $0x401120,%edi\nmov $48,%ecx\n1: cmpq $0,(%rdi)\njne 9f\nadd $8,%edi\nloop 1b\nmovzbl 0x401000,%r12d\nmov %r12,0x401010\nmov %r12d,%r13d\nshl $10,%r13d\nor $0x37f,%r13d\nmov %r13w,0x401020\nfldcw 0x401020\nmov %r12d,%r14d\nshl $13,%r14d\nor $0x1f80,%r14d\nmov %r14d,0x401024\nldmxcsr 0x401024\nfildq 0x401010\nmovq %r12,%xmm0\npunpcklqdq %xmm0,%xmm0\nmovq %r13,%xmm7\npunpcklqdq %xmm7,%xmm7\nmovq %r14,%xmm15\npunpcklqdq %xmm15,%xmm15\n2: call 7f\nincq 0x401008\ncmpb $0,0x401018\nje 2b\nmov $8,%eax\nint $0x80\ncall 7f\nmov $11,%eax\nmov $3,%edi\nint $0x80\ntest %rax,%rax\njne 9f\ncall 7f\nmov $2,%eax\nlea 6f(%rip),%rdi\nmov $14,%esi\nxor %edx,%edx\nmov $0x401400,%ecx\nmov $16,%r8d\nint $0x80\ncmp $16,%rax\njne 9f\ncall 7f\nfninit\nfldz\nfldz\nfdivp\nmovw $0x37e,0x401020\nfldcw 0x401020\nmovl $0x6001,0x401024\nldmxcsr 0x401024\nmov $8,%eax\nint $0x80\nfnstsw %ax\ntest $1,%ax\nje 9f\nfnstcw 0x401020\ncmpw $0x37e,0x401020\njne 9f\nstmxcsr 0x401024\ncmpl $0x6001,0x401024\njne 9f\nfnclex\nfninit\nmovl $0x1f80,0x401024\nldmxcsr 0x401024\nmovw $0x37e,0x401020\nfldcw 0x401020\nmov $8,%ecx\n4: fld1\nloop 4b\nmov $8,%eax\nint $0x80\nfxsave64 0x401100\ncmpw $0x37e,0x401100\njne 9f\ncmpw $0,0x401102\njne 9f\ncmpb $255,0x401104\njne 9f\nmov $0x401120,%edi\nmov $8,%ecx\nmovabs $0x8000000000000000,%rax\n4: cmp %rax,(%rdi)\njne 9f\ncmpw $0x3fff,8(%rdi)\njne 9f\nadd $16,%edi\nloop 4b\nfninit\nmovq %r12,%mm0\nmovq %r13,%mm7\nmov $8,%eax\nint $0x80\n3: movq %mm0,%rax\ncmp %r12,%rax\njne 9f\nmovq %mm7,%rax\ncmp %r13,%rax\njne 9f\nincq 0x401030\ncmpb $0,0x401038\nje 3b\nemms\nmov $42,%edi\njmp 8f\n7: movdqa %xmm0,%xmm6\npsrldq $8,%xmm6\nmovq %xmm6,%rax\ncmp %r12,%rax\njne 9f\nmovdqa %xmm7,%xmm6\npsrldq $8,%xmm6\nmovq %xmm6,%rax\ncmp %r13,%rax\njne 9f\nmovdqa %xmm15,%xmm6\npsrldq $8,%xmm6\nmovq %xmm6,%rax\ncmp %r14,%rax\njne 9f\nmovq %xmm0,%rax\ncmp %r12,%rax\njne 9f\nmovq %xmm7,%rax\ncmp %r13,%rax\njne 9f\nmovq %xmm15,%rax\ncmp %r14,%rax\njne 9f\nfld %st(0)\nfistpq 0x401028\ncmp %r12,0x401028\njne 9f\nfnstcw 0x401020\ncmp %r13w,0x401020\njne 9f\nstmxcsr 0x401024\ncmp %r14d,0x401024\njne 9f\nret\n9: mov $13,%edi\n8: xor %eax,%eax\nint $0x80\nud2\n6: .ascii \"test/immutable\"\n.global fxend\nfxend:\n");}
static int fxprogress(int a,int b,u32 off){volatile u64*p=(volatile u64*)xl(slots[a].cr3,UB+PG+off,1),*q=(volatile u64*)xl(slots[b].cr3,UB+PG+off,1);if(!p||!q)return 0;for(u32 i=0;i<4;++i){u64 x=*p,y=*q,start=tnow();while((*p==x||*q==y)&&tnow()-start<THZ){if(slots[a].st==ZOM||slots[b].st==ZOM)return 0;__asm__ volatile("pause");}if(*p==x||*q==y)return 0;}return 1;}
extern const u8 fpxm[],fpend[];__attribute__((naked,noinline,used))static void fpbad(void){__asm__ volatile("sub $16,%rsp\nfninit\nfldz\nfldz\nfdivp\nmovw $0x37e,(%rsp)\nfldcw (%rsp)\nfwait\njmp 1f\n.global fpxm\nfpxm: sub $16,%rsp\nmovl $0,(%rsp)\nldmxcsr (%rsp)\nxorps %xmm0,%xmm0\ndivss %xmm0,%xmm0\n1: mov $13,%edi\nxor %eax,%eax\nint $0x80\nud2\n.global fpend\nfpend:\n");}
static int fxt(void){u8 base[512]__attribute__((aligned(16)));__asm__ volatile("call fxreset\nfxrstor64 fxclean(%%rip)\nfxsave64 %0":"=m"(base)::"memory");u8 z[2048];u32 ncode=(u32)(fxend-(const u8*)fxwork);if(ncode>sizeof(z)-89u)return 0;for(u32 pass=0;pass<2;++pass){u64 free=mfc();u32 n=xi(z,(const u8*)fxwork,ncode);struct xs*v=(void*)(z+sizeof(struct xh));z[v[1].off]=1;int a=t2(z,n);z[v[1].off]=2;int b=t2(z,n),busy=tuser(workup,sizeof(workup),0);if(a<0||b<0||busy<0)return 0;fxbad=fxprobes=0;fxchecking=1;if(!fxprogress(a,b,8))return 0;for(u32 j=6;j<24;++j)if(*xl(slots[a].cr3,UB+PG+256+j,0)!=base[j]||*xl(slots[b].cr3,UB+PG+256+j,0)!=base[j])return 0;*xl(slots[a].cr3,UB+PG+24,1)=1;*xl(slots[b].cr3,UB+PG+24,1)=1;if(!fxprogress(a,b,48)||fxbad||fxprobes!=2||!slots[a].pr||!slots[b].pr)return 0;*xl(slots[a].cr3,UB+PG+56,1)=1;*xl(slots[b].cr3,UB+PG+56,1)=1;if(!aw((u32)a,42)||!aw((u32)b,42))return 0;if(!pass){u32 sz=(u32)(fpend-(const u8*)fpbad);a=tuser((const u8*)fpbad,sz,0);b=tuser((const u8*)fpbad,sz,(u32)(fpxm-(const u8*)fpbad));if(a<0||b<0||!aw((u32)a,UINT64_MAX)||!aw((u32)b,UINT64_MAX))return 0;volatile u64*p=(volatile u64*)xl(slots[busy].cr3,UB+256,1);u64 x=*p,start=tnow();while(*p==x&&tnow()-start<THZ)__asm__ volatile("pause");if(*p==x)return 0;}if(!tkill((u32)busy)||mfc()!=free)return 0;fxchecking=0;}static const u8 avx[]={0xc5,0xf8,0x57,0xc0,0xbf,13,0,0,0,0x31,0xc0,0xcd,0x80},pkru[]={0x31,0xc9,0x0f,0x01,0xee,0xbf,13,0,0,0,0x31,0xc0,0xcd,0x80};u64 free=mfc();int a=tuser(avx,sizeof(avx),0);if(a<0||!aw((u32)a,UINT64_MAX))return 0;a=tuser(pkru,sizeof(pkru),0);return a>=0&&aw((u32)a,UINT64_MAX)&&mfc()==free;}
#include "task_wait_tests.h"
#include "sdk_tests.h"
#include "args_tests.h"
#include "libc_tests.h"
#include "input_tests.h"
#include "enum_tests.h"
#include "console_tests.h"
int tinit(void){if(!nxok())return 0;u64 f=lock();for(u32 i=0;i<NT;++i){cz(&slots[i].c);slots[i].st=slots[i].ir=slots[i].pr=0;slots[i].waiter=slots[i].waitfor=0;slots[i].cr3=slots[i].hb=slots[i].brk=slots[i].xs=slots[i].wake=0;}cur=0;itstack(stacks[0]+KS);slots[0].cr3=gc();slots[0].st=RUN;ready=1;unlock(f);kran=0;int kt=tnew(kfn);u64 kt_start=tnow();while(!kran&&tnow()-kt_start<THZ)__asm__ volatile("pause");if(kt<0||!kran)return 0;if(!imt()||!x2()||!slt()||!fxt()||!wait_tests())return 0;int first=tuser(workup,sizeof(workup),0),second=tuser(workdown,sizeof(workdown),0);if(first<0||second<0){if(first>0)(void)tkill((u32)first);return 0;}volatile u64*a=(volatile u64*)xl(slots[first].cr3,UB+0x100,1),*b=(volatile u64*)xl(slots[second].cr3,UB+0x100,1);u64 start=tnow();while((*a==0||*b==0)&&tnow()-start<THZ)__asm__ volatile("pause");int success=*a&&*b;(void)tkill((u32)first);(void)tkill((u32)second);if(!success)return 0;u64 av=mfc();int fault=tuser((const u8*)"\x0f\x0b",2,0);if(fault<0)return 0;u64 root=slots[fault].cr3;start=tnow();while(!dn((u32)fault,UINT64_MAX)&&tnow()-start<THZ)__asm__ volatile("pause");if(mfc()!=av)return 0;u64 ru=mpa();if(ru!=root){if(ru)(void)mpf(ru);return 0;}(void)mpf(ru);av=mfc();static const u8 path[]="bin/abi-test.cxe",made[]="test/user",child[]="bin/child.cxe";if(!fw(child,sizeof(child)-1,0,cx,sizeof(cx))||!fw(path,sizeof(path)-1,0,workhead,sizeof(workhead)))return 0;u32 iz=2u*PG,czs=(u32)(abiend-(const u8*)abitest);if(czs>PG-0x80u||!ft(path,sizeof(path)-1,EH+iz)||!fw(path,sizeof(path)-1,EH+0xff8,path,sizeof(path)-1)||!fw(path,sizeof(path)-1,EH+0x1040,path,sizeof(path)-1)||!fw(path,sizeof(path)-1,EH+0x1060,(const u8*)"test/immutable",14)||!fw(path,sizeof(path)-1,EH+0x1070,made,sizeof(made)-1)||!fw(path,sizeof(path)-1,EH+0x1010,child,sizeof(child)-1)||!fw(path,sizeof(path)-1,EH+0x1080,(const u8*)abitest,czs))return 0;int exiting=tfile(path,sizeof(path)-1);if(exiting<0)return 0;int busy=tuser(workup,sizeof(workup),0);if(busy<0)return 0;root=slots[exiting].cr3;start=tnow();while(!dn((u32)exiting,42)&&tnow()-start<THZ)__asm__ volatile("pause");ru=mpa();u32 vp;u8*vd=vtarget(0,0,2,1,&vp);int pixels=vd&&((u32*)vd)[0]==0x112233&&((u32*)vd)[1]==0x445566;if(vd)((u32*)vd)[0]=((u32*)vd)[1]=0;int rc=ru==root,frd=!ru||mpf(ru),co=slots[exiting].pr,so=tkill((u32)busy);int rm=fr(path,sizeof(path)-1),mr=fr(made,sizeof(made)-1),cr=fr(child,sizeof(child)-1);return pixels&&rc&&frd&&co&&so&&rm&&mr&&cr&&mfc()==av&&sdk_tests()&&args_tests()&&libc_tests()&&input_sys_tests()&&enumeration_tests()&&console_tests();}
