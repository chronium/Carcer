#pragma GCC target("general-regs-only")
#include "files.h"
#include "heap.h"
#include "memory.h"
typedef unsigned char u8;typedef unsigned short u16;typedef unsigned u32;typedef unsigned long u64;
int fu(const u8*p,u32 n){u32 i=0;while(i<n){u8 f=p[i++];u32 c,m,r;if(f<=0x7f)continue;if(f>=0xc2&&f<=0xdf)c=f&31,m=0x80,r=1;else if(f>=0xe0&&f<=0xef)c=f&15,m=0x800,r=2;else if(f>=0xf0&&f<=0xf4)c=f&7,m=0x10000,r=3;else return 0;if(r>n-i)return 0;while(r--){u8 q=p[i++];if((q&0xc0)!=0x80)return 0;c=(c<<6)|(q&63);}if(c<m||c>0x10ffff||(c>=0xd800&&c<=0xdfff))return 0;}return 1;}int fpv(const u8*p,u32 n){return p&&n&&n<=FPL&&fu(p,n);}struct file files[FMC];u32 fc;static struct file detached[FMC];static u64 file_serial;static void cp(u8*d,const u8*s,u32 n){for(u32 i=0;i<n;++i)d[i]=s[i];}static void cpf(struct file*d,const struct file*s){cp(d->p,s->p,s->n);d->n=s->n;d->d=s->d;d->z=s->z;d->c=s->c;d->a=s->a;d->refs=s->refs;d->id=s->id;}static int eq(const u8*a,u32 n,const u8*b,u32 m){if(n!=m)return 0;for(u32 i=0;i<n;++i)if(a[i]!=b[i])return 0;return 1;}static int cmp(const u8*a,u32 n,const u8*b,u32 m){u32 z=n<m?n:m;for(u32 i=0;i<z;++i){if(a[i]<b[i])return-1;if(a[i]>b[i])return 1;}return n<m?-1:n>m;}static u64 file_allocation_budget=UINT64_MAX;static int reserve(struct file*f,u32 n){if(n<=f->c)return 1;if(file_allocation_budget!=UINT64_MAX){if(!file_allocation_budget)return 0;--file_allocation_budget;}u32 z=f->c;if(z<256)z=256;while(z<n){if(z>UINT32_MAX/2){z=n;break;}z*=2;}u8*p=ha(z);if(!p)return 0;cp(p,f->d,f->z);if(f->d&&!hf(f->d)){(void)hf(p);return 0;}f->d=p;f->c=z;return 1;}static int create(const u8*p,u32 n,const u8*d,u32 z){if(!fpv(p,n)||fc>=FMC||file_serial==UINT64_MAX-1)return 0;u32 at=0;while(at<fc){int q=cmp(files[at].p,files[at].n,p,n);if(!q)return 0;if(q>0)break;++at;}struct file f;f.n=(u16)n;f.d=0;f.z=f.c=f.a=f.refs=0;f.id=0;cp(f.p,p,n);if(z){if(!reserve(&f,z))return 0;cp(f.d,d,z);f.z=z;}f.id=++file_serial;for(u32 i=fc;i>at;--i)cpf(&files[i],&files[i-1]);cpf(&files[at],&f);++fc;return 1;}static void discard(void){while(fc){--fc;if(files[fc].d)(void)hf(files[fc].d);}}int fi(void){fc=0;if(initial_file_count>FMC)return 0;for(u32 i=0;i<initial_file_count;++i){const struct embedded_file*f=&initial_files[i];uintptr_t n=(uintptr_t)f->end-(uintptr_t)f->data;if(n>UINT32_MAX||!create(f->p,f->n,f->data,(u32)n)){discard();return 0;}}return 1;}u32 fz(const struct file*f){return f->z;}const u8*fd(const struct file*f){return f->d;}u32 fa(const struct file*f){return f->a;}struct file*ff(const u8*p,u32 n){for(u32 i=0;i<fc;++i)if(eq(p,n,files[i].p,files[i].n))return&files[i];return 0;}int fpp(const struct file*f,const u8*p,u32 n){return n<=f->n&&eq(f->p,n,p,n);}static int resize(struct file*f,u32 n,int zero){u32 old=f->z;if(n==old)return 1;if(!n){if(f->d&&!hf(f->d))return 0;f->d=0;f->z=f->c=0;return 1;}if(!reserve(f,n))return 0;if(zero&&n>old)for(u32 i=old;i<n;++i)f->d[i]=0;f->z=n;return 1;}int fws(const u8*p,u32 n,u32 off,u32 z,u8**out){struct file*f=ff(p,n);if(!f){if(off||!create(p,n,0,0))return 0;f=ff(p,n);}if(!f||(f->a&FIM)||off>f->z||z>UINT32_MAX-off)return 0;if(z&&off+z>f->z&&!resize(f,off+z,0))return 0;*out=z?f->d+off:0;return 1;}int fw(const u8*p,u32 n,u32 off,const u8*d,u32 z){u8*q;if(!fws(p,n,off,z,&q))return 0;cp(q,d,z);return 1;}int ft(const u8*p,u32 n,u32 z){struct file*f=ff(p,n);return f&&!(f->a&FIM)&&resize(f,z,1);}int fr(const u8*p,u32 n){struct file*f=ff(p,n);if(!f||(f->a&FIM))return 0;u32 at=(u32)(f-files);if(f->refs){u32 j=0;while(j<FMC&&detached[j].id)++j;if(j==FMC)return 0;cpf(&detached[j],f);}else if(f->d&&!hf(f->d))return 0;for(u32 i=at;i+1<fc;++i)cpf(&files[i],&files[i+1]);--fc;return 1;}int fsl(const u8*p,u32 n){struct file*f=ff(p,n);if(!f)return 0;f->a|=FIM;return 1;}

/* Caller serializes namespace mutations. Rename transfers the allocation,
 * including when the table is full; neither supplied endpoint may be sealed. */
int fcreate(const u8 *p,u32 n) { return create(p,n,0,0); }
int fmove(const u8 *p,u32 n,const u8 *q,u32 m) {
    if(!fpv(q,m)) return 0;
    struct file *src=ff(p,n),*dst=ff(q,m);
    if(!src || (src->a&FIM) || (dst && (dst->a&FIM))) return 0;
    if(src==dst) return 1;
    if(dst && !fr(q,m)) return 0;
    src=ff(p,n);
    struct file saved;
    cpf(&saved,src);
    u32 at=(u32)(src-files);
    for(u32 i=at;i+1<fc;++i) cpf(&files[i],&files[i+1]);
    --fc;
    saved.n=(u16)m;
    cp(saved.p,q,m);
    at=0;
    while(at<fc && cmp(files[at].p,files[at].n,q,m)<0) ++at;
    for(u32 i=fc;i>at;--i) cpf(&files[i],&files[i-1]);
    cpf(&files[at],&saved);
    ++fc;
    return 1;
}

/* No stored pointer into files[] survives a namespace operation. Detached
 * records own the same allocation until their final handle reference closes. */
struct file *fbyid(u64 id) {
    if(!id)return 0;
    for(u32 i=0;i<fc;i++)if(files[i].id==id)return &files[i];
    for(u32 i=0;i<FMC;i++)if(detached[i].id==id)return &detached[i];
    return 0;
}
u64 fhold(struct file *f) {
    if(!f || !f->id || f->refs==UINT32_MAX)return 0;
    ++f->refs;return f->id;
}
int fdrop(u64 id) {
    struct file *f=fbyid(id);
    if(!f || !f->refs)return 0;
    if(f->refs==1) {
        for(u32 i=0;i<FMC;i++)if(f==&detached[i]) {
            if(f->d&&!hf(f->d))return 0;
            f->id=0;f->refs=0;f->d=0;f->z=f->c=0;
            return 1;
        }
    }
    --f->refs;return 1;
}
int fresize(struct file *f,u32 n) {
    return f && !(f->a&FIM) && resize(f,n,1);
}
int fprepare(struct file *f,u32 off,u32 n,u8 **out) {
    if(!f || (f->a&FIM) || n>UINT32_MAX-off)return 0;
    if(!n){*out=0;return 1;}
    u32 old=f->z,end=off+n;
    if(end>old && !reserve(f,end))return 0;
    for(u32 i=old;i<off;i++)f->d[i]=0;
    if(end>old)f->z=end;
    *out=f->d+off;return 1;
}
u32 forphans(void) {
    u32 n=0;for(u32 i=0;i<FMC;i++)if(detached[i].id)++n;return n;
}

/* Boot-only failure injection. No token is issued while serial is altered. */
int fobject_tests(void) {
    static const u8 p[]="test/fobj-a",q[]="test/fobj-b";
    u32 count=fc;u64 pages=mfc(),serial=file_serial,id=0;
    int ok=0;
    if(ff(p,sizeof(p)-1)||ff(q,sizeof(q)-1)||forphans())return 0;
    if(!fw(p,sizeof(p)-1,0,(const u8 *)"data",4))goto done;
    struct file *f=ff(p,sizeof(p)-1);
    id=fhold(f);if(!id)goto done;
    const u8 *old=f->d;u32 capacity=f->c;
    u8 *out=(u8 *)(uintptr_t)1;
    file_allocation_budget=0;
    if(fprepare(f,8192,1,&out)||fresize(f,8192)||f->d!=old||
       f->z!=4||f->c!=capacity||out!=(u8 *)(uintptr_t)1||
       !eq(f->d,4,(const u8 *)"data",4))goto done;
    serial=file_serial;
    if(create(q,sizeof(q)-1,(const u8 *)"x",1)||fc!=count+1||file_serial!=serial)goto done;
    file_allocation_budget=UINT64_MAX;
    file_serial=UINT64_MAX-1;
    int made=fcreate(q,sizeof(q)-1);
    file_serial=serial;
    if(made||fc!=count+1)goto done;
    if(!fsl(p,sizeof(p)-1))goto done;
    int sealed=fprepare(f,0,1,&out)||fresize(f,0)||fr(p,sizeof(p)-1);
    f->a=0;
    if(sealed||f->z!=4||f->d[0]!='d')goto done;
    if(!fr(p,sizeof(p)-1)||forphans()!=1||fbyid(id)==0)goto done;
    if(!fcreate(p,sizeof(p)-1)||ff(p,sizeof(p)-1)->id==id)goto done;
    if(!fdrop(id)||fbyid(id)||forphans())goto done;
    id=0;ok=1;
done:
    file_allocation_budget=UINT64_MAX;
    if(id)(void)fdrop(id);
    if(ff(p,sizeof(p)-1)&&!fr(p,sizeof(p)-1))ok=0;
    if(ff(q,sizeof(q)-1)&&!fr(q,sizeof(q)-1))ok=0;
    return ok&&fc==count&&mfc()==pages&&!forphans();
}
