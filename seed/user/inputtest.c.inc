#include "cx.h"
/* Boot observer makes the middle .probe page read-only before scheduling us. */
__attribute__((section(".probe"),aligned(4096))) unsigned char pages[12288]={[4096]=0x5a};
#define C(x) do{if(!(x))cx_exit(__LINE__);}while(0)
static int filled(const void *p,size_t n,unsigned char c){const unsigned char *s=p;for(size_t i=0;i<n;i++)if(s[i]!=c)return 0;return 1;}
int main(void) {
    C(cx_call(15,UINT64_MAX,0,UINT64_MAX,0,0,0)==1);
    unsigned char *p=cx_alloc(12288);C(p);
    struct cx_key_event *events=(void *)(p+4096-8);
    uint64_t *cursor=(void *)(p+8192-4);*cursor=0;
    C(cx_keys(events,64,cursor)==2&&*cursor==3);
    C(events[0].sequence==1&&events[0].tick==123&&events[0].code==0x1e&&events[0].pressed==1&&events[0].flags==0&&events[0].reserved==0);
    C(events[1].sequence==2&&events[1].tick==124&&events[1].code==0x1e&&events[1].pressed==0);
    memset(events,0xa5,64*sizeof(*events));
    C(cx_keys(events,64,cursor)==0&&*cursor==3&&filled(events,64*sizeof(*events),0xa5));
    uint64_t other=1;
    C(cx_keys(events,1,&other)==1&&other==2&&*cursor==3&&events[0].sequence==1);
    memset(events,0xa5,64*sizeof(*events));
    C(cx_keys(events,65,cursor)==CX_ERROR&&*cursor==3);
    C(cx_keys(events,UINT64_MAX,cursor)==CX_ERROR&&*cursor==3);
    C(cx_keys(events,1ull<<63,cursor)==CX_ERROR&&*cursor==3);
    C(cx_keys((void *)1,1,cursor)==CX_ERROR&&*cursor==3);
    C(cx_keys(events,1,(void *)1)==CX_ERROR);
    C(filled(events,64*sizeof(*events),0xa5));
    unsigned char *end=(void *)(uintptr_t)cx_brk(0);
    memset(end-8,0x6b,8);
    C(cx_keys((void *)(end-8),1,cursor)==CX_ERROR&&*cursor==3&&filled(end-8,8,0x6b));
    memset(pages+4096-8,0x7c,8);
    C(cx_keys((void *)(pages+4096-8),1,cursor)==CX_ERROR&&*cursor==3);
    C(filled(pages+4096-8,8,0x7c)&&pages[4096]==0x5a);
    C(cx_keys(events,1,(void *)(pages+4096))==CX_ERROR&&pages[4096]==0x5a&&filled(events,64*sizeof(*events),0xa5));
    *cursor=UINT64_MAX;
    C(cx_keys(events,64,cursor)==CX_ERROR&&*cursor==UINT64_MAX&&filled(events,64*sizeof(*events),0xa5));
    *cursor=3;
    uint64_t *overlap=(void *)((char *)events+8);*overlap=3;
    unsigned char copy[24];memcpy(copy,events,24);
    C(cx_keys(events,1,overlap)==CX_ERROR&&!memcmp(copy,events,24));
    memset(events,0xa5,64*sizeof(*events));
    C(cx_keys(events,1,(void *)(end-4))==CX_ERROR&&filled(events,64*sizeof(*events),0xa5));
    return 0;
}
