#include "cxlib.h"
int errno;
/* First-fit allocator, 16-byte payload alignment, address-ordered block chain.
 * Reuses and coalesces freed blocks. brk pages are retained until task exit.
 * cx_alloc remains the sole underlying break manager. Single-threaded ABI. */
typedef struct block { size_t size; struct block *next; uint64_t used,pad; } block;
static block *head;
static void split(block *b,size_t n) {
    if(b->size-n>=sizeof(block)+16) {
        block *r=(void *)((char *)(b+1)+n);
        r->size=b->size-n-sizeof(block);r->used=0;r->pad=0;
        r->next=b->next;b->next=r;b->size=n;
    }
}
void *malloc(size_t n) {
    if(!n)n=1;
    if(n>SIZE_MAX-15-sizeof(block)){errno=ENOMEM;return NULL;}
    n=(n+15)&~(size_t)15;
    block *tail=NULL;
    for(block *b=head;b;b=b->next) {
        if(!b->used && b->size>=n){split(b,n);b->used=1;return b+1;}
        tail=b;
    }
    size_t total=n+sizeof(block);
    if(total<65536)total=65536;
    block *b=cx_alloc(total);
    if(!b){errno=ENOMEM;return NULL;}
    b->size=total-sizeof(block);b->next=NULL;b->used=0;b->pad=0;
    if(tail)tail->next=b;else head=b;
    split(b,n);b->used=1;return b+1;
}
void free(void *p) {
    if(!p)return;
    block *b=(block *)p-1;b->used=0;
    for(b=head;b && b->next;) {
        block *q=b->next;
        if(!b->used && !q->used && (char *)(b+1)+b->size==(char *)q) {
            b->size+=sizeof(block)+q->size;b->next=q->next;
        } else b=q;
    }
}
void *calloc(size_t n,size_t z) {
    if(z && n>SIZE_MAX/z){errno=ENOMEM;return NULL;}
    size_t size=n*z;void *p=malloc(size);
    if(p)memset(p,0,size);
    return p;
}
void *realloc(void *p,size_t n) {
    if(!p)return malloc(n);
    if(!n){free(p);return NULL;}
    block *b=(block *)p-1;
    if(n<=b->size)return p;
    void *q=malloc(n);
    if(q){memcpy(q,p,b->size);free(p);}
    return q;
}
static void (*at_exit[32])(void);
static unsigned at_count;
int atexit(void (*f)(void)) {
    if(!f || at_count==32)return -1;
    at_exit[at_count++]=f;return 0;
}
void exit(int status) {
    while(at_count)at_exit[--at_count]();
    fflush(NULL);cx_exit((uint32_t)status);
}
void abort(void) {cx_exit(134);}
int abs(int n){return n<0?-n:n;}
long labs(long n){return n<0?-n:n;}
double fabs(double x){union {double d;uint64_t u;} v={x};v.u&=UINT64_MAX>>1;return v.d;}
char *getenv(const char *name){(void)name;return NULL;}
int system(const char *command){if(!command)return 0;errno=ENOSYS;return -1;}
int mkdir(const char *path,unsigned mode){(void)path;(void)mode;errno=ENOSYS;return -1;}
