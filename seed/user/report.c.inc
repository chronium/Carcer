#include "cx.h"
static const char child[]="seed/user/child.cxe";
static const char output[]="test/sdk-result";
int main(void) {
    unsigned char *b=cx_alloc(8193);
    if(!b || ((uintptr_t)b&15)) return 1;
    for(size_t i=0;i<8193;i++) if(b[i]) return 2;
    for(size_t i=0;i<8193;i++) b[i]=(unsigned char)(i*17+3);
    if(cx_write(output,sizeof(output)-1,0,b,8193)!=8193) return 3;
    memset(b,0,8193);
    if(cx_size(output,sizeof(output)-1)!=8193 ||
       cx_read(output,sizeof(output)-1,0,b,8193)!=8193) return 4;
    for(size_t i=0;i<8193;i++) if(b[i]!=(unsigned char)(i*17+3)) return 5;
    if(cx_attributes("test/immutable",14)!=1 ||
       cx_write("test/immutable",14,0,b,1)!=CX_ERROR) return 6;
    uint64_t id=cx_spawn(child,sizeof(child)-1),status=0;
    if(id==CX_ERROR || cx_wait(id,&status)!=1 || status!=37) return 7;
    if(cx_reap(id,&status)!=CX_ERROR) return 8;
    uint64_t t=cx_ticks();
    if(cx_sleep(5)!=0 || cx_ticks()-t<5) return 9;
    struct cx_display v;
    if(cx_display_info(&v)!=32 || v.format!=1 || !v.width || !v.height) return 10;
    return 42;
}
