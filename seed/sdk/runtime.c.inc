#include "cx.h"
void *memcpy(void *dst,const void *src,size_t n) {
    unsigned char *d=dst; const unsigned char *s=src;
    for(size_t i=0;i<n;i++) d[i]=s[i];
    return dst;
}
void *memmove(void *dst,const void *src,size_t n) {
    unsigned char *d=dst; const unsigned char *s=src;
    if((uintptr_t)d<(uintptr_t)s) return memcpy(dst,src,n);
    while(n) { --n; d[n]=s[n]; }
    return dst;
}
void *memset(void *dst,int c,size_t n) {
    unsigned char *d=dst;
    for(size_t i=0;i<n;i++) d[i]=(unsigned char)c;
    return dst;
}
int memcmp(const void *a,const void *b,size_t n) {
    const unsigned char *x=a,*y=b;
    for(size_t i=0;i<n;i++) if(x[i]!=y[i]) return (int)x[i]-(int)y[i];
    return 0;
}
size_t strlen(const char *s) { size_t n=0; while(s[n]) ++n; return n; }
void *cx_alloc(size_t n) {
    static uintptr_t next,end;
    if(!n || n>SIZE_MAX-15) return 0;
    n=(n+15)&~(size_t)15;
    if(!next) {
        uint64_t base=cx_brk(0);
        if(base==CX_ERROR) return 0;
        next=end=base;
    }
    if(n>UINTPTR_MAX-next) return 0;
    uintptr_t want=next+n;
    if(want>end) {
        if(want>UINTPTR_MAX-4095) return 0;
        uintptr_t rounded=(want+4095)&~(uintptr_t)4095;
        if(cx_brk(rounded)!=rounded) return 0;
        end=rounded;
    }
    void *p=(void *)next; next=want; return p;
}
