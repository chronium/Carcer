#include "cx.h"
static const char child[]="seed/user/argchild.cxe";
static int waitfor(uint64_t task) {
    uint64_t result;
    return task!=CX_ERROR && cx_wait(task,&result)==1 && result==73;
}
static int denied(const struct cx_arg *args,size_t n) {
    return cx_spawn_args(child,sizeof(child)-1,args,n)==CX_ERROR;
}
int main(void) {
    if(!waitfor(cx_spawn_args(child,sizeof(child)-1,(void *)UINTPTR_MAX,0))) return 1;
    char *big=cx_alloc(4097);
    if(!big) return 2;
    memset(big,'q',4097);
    struct cx_arg one={big,4095};
    uint64_t task=cx_spawn_args(child,sizeof(child)-1,&one,1);
    big[0]='y';
    if(!waitfor(task) || big[0]!='y') return 3;
    big[0]='q';
    one.size=4096;
    if(!denied(&one,1)) return 4;
    one.size=UINT64_MAX;
    if(!denied(&one,1)) return 5;
    one.data=(const char *)0x3fffffff;one.size=2;
    if(!denied(&one,1)) return 6;
    one.data="a\0b";one.size=3;
    if(!denied(&one,1)) return 7;
    if(!denied((void *)UINTPTR_MAX,1) ||
       !denied((void *)0x3ffffff8,1) ||
       !denied(0,33) || !denied(0,SIZE_MAX)) return 8;
    if(cx_spawn_args(0,1,0,0)!=CX_ERROR ||
       cx_spawn_args(child,0,0,0)!=CX_ERROR ||
       cx_spawn_args("absent/args.cxe",15,0,0)!=CX_ERROR ||
       cx_spawn_args("test/immutable",14,0,0)!=CX_ERROR) return 9;
    /* Deliberately cross a mapped page boundary with both the span vector and
     * a string. All pointer objects themselves remain naturally aligned. */
    char *storage=cx_alloc(16384);
    if(!storage) return 10;
    uintptr_t base=((uintptr_t)storage+4095)&~(uintptr_t)4095;
    struct cx_arg *args=(void *)(base+4096-8);
    char *text=(void *)(base+8192-2);
    memcpy(text,"a b\t",4);
    args[0]=(struct cx_arg){"argchild",8};
    args[1]=(struct cx_arg){(void *)UINTPTR_MAX,0};
    args[2]=(struct cx_arg){text,4};
    args[3]=(struct cx_arg){"\xff\x80",2};
    for(unsigned i=4;i<31;++i) args[i]=(struct cx_arg){"x",1};
    args[31]=(struct cx_arg){big,4023};
    task=cx_spawn_args(child,sizeof(child)-1,args,32);
    big[0]='y';text[0]='z';
    args[0].data=0; /* Child vector is also independent of its parent's. */
    if(!waitfor(task) || big[0]!='y' || text[0]!='z') return 11;
    args[0]=(struct cx_arg){"argchild",8};
    args[31].size=4024;
    if(!denied(args,32)) return 12;
    /* Repeated successful and rejected launches must not consume task slots. */
    for(unsigned i=0;i<12;++i) {
        if(!denied(0,1) ||
           !waitfor(cx_spawn_args(child,sizeof(child)-1,0,0))) return 13;
    }
    return 0;
}
