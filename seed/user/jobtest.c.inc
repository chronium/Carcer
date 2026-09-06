#include "../sdk/cx.h"
#define C(x) do{if(!(x))cx_exit(__LINE__);}while(0)
/* Boot observer may fill raw-slot for additional legacy-interface checks.
 * Without an observer, all normal lifecycle tests still run standalone. */
volatile uint64_t probe[8] __attribute__((section(".probe"),aligned(4096)));
static unsigned char cross[8192] __attribute__((aligned(4096)));
static const char path[]="seed/user/jobtest.cxe",ready[]="test/job-ready";
static int eq(const char *a,const char *b){size_t n=strlen(a);return n==strlen(b)&&!memcmp(a,b,n);}
static uint64_t dec(const char *s){
    uint64_t n=0;for(;*s;s++){C(*s>='0'&&*s<='9');n=n*10+(unsigned)(*s-'0');}return n;
}
static void num(char *s,uint64_t n){
    char t[32];unsigned z=0;do{t[z++]=(char)('0'+n%10);n/=10;}while(n);
    for(unsigned i=0;i<z;i++)s[i]=t[z-i-1];
    s[z]=0;
}
static uint64_t spawn(const char *mode,const char *value){
    struct cx_arg a[]={{path,sizeof(path)-1},{mode,strlen(mode)},{value,0}};
    if(value)a[2].size=strlen(value);
    return cx_job_spawn(path,sizeof(path)-1,a,value?3:2);
}
static void ready_wait(void){
    uint64_t start=cx_ticks();
    while(cx_size(ready,sizeof(ready)-1)==CX_ERROR){
        C(cx_ticks()-start<500);C(cx_sleep(1)==0);
    }
}
static void no_result(uint64_t job){
    uint64_t status=0x123456789abcdef0ull;
    C(cx_job_poll(job,&status)==CX_ERROR&&status==0x123456789abcdef0ull);
    C(cx_job_wait(job,&status)==CX_ERROR&&status==0x123456789abcdef0ull);
    C(cx_job_stop(job,&status)==CX_ERROR&&status==0x123456789abcdef0ull);
}
static void invalid_buffers(uint64_t job){
    static const uint64_t ro=0x9876543210abcdefull;
    const uint64_t bad[]={1,(uintptr_t)&ro,0x3ffffffc,UINT64_MAX};
    for(unsigned op=0;op<3;op++)for(unsigned i=0;i<4;i++)
        C(cx_call(18,job,op,bad[i],0,0,0)==CX_ERROR);
    C(ro==0x9876543210abcdefull);
    uint64_t end=cx_brk(0);C(end!=CX_ERROR&&cx_brk(end+4096)==end+4096);
    volatile unsigned char *tail=(void *)(uintptr_t)(end+4092);
    for(unsigned i=0;i<4;i++)tail[i]=0x5a;
    for(unsigned op=0;op<3;op++){
        C(cx_call(18,job,op,(uintptr_t)tail,0,0,0)==CX_ERROR);
        for(unsigned i=0;i<4;i++)C(tail[i]==0x5a);
    }
    C(cx_brk(end)==end);
}
/* Explicit boot-only modes wait for an observer gate in private user memory.
 * The ordinary no-argument suite above/below never needs these gates. */
static void gate(uint64_t value){
    while(probe[7]<value)C(cx_sleep(1)==0);
}
int main(int argc,char **argv){
    uint64_t h,status=0;
    if(argc>1){
        const char *mode=argv[1];
        if(eq(mode,"controlled")){
            C(argc==3);unsigned action=(unsigned)(argv[2][0]-'0');
            probe[0]=10;gate(1);
            h=spawn("controlled-owner",argv[2]);C(h!=CX_ERROR);
            probe[1]=h;probe[0]=11;gate(2);
            if(action<2)C(cx_job_wait(h,&status)==1&&status==(action?UINT64_MAX:41));
            else C(cx_job_stop(h,&status)==2&&status==UINT64_MAX);
            probe[0]=12;gate(3);no_result(h);return 0;
        }
        if(eq(mode,"controlled-owner")){
            C(argc==3&&strlen(argv[2])>=3);
            h=spawn("controlled-branch",argv[2]+2);C(h!=CX_ERROR);
            uint64_t zombie=spawn("exit","91");C(zombie!=CX_ERROR);
            probe[1]=h;probe[2]=zombie;probe[0]=1;gate(1);
            probe[0]=2;
            if(argv[2][0]=='0')return 41;
            if(argv[2][0]=='1'){__asm__ volatile("ud2");return 99;}
            if(argv[2][0]=='2'){C(cx_job_wait(h,&status)==1);return 99;}
            C(cx_sleep(10000)==0);return 99;
        }
        if(eq(mode,"controlled-branch")){
            C(argc==3);h=spawn("spin",0);C(h!=CX_ERROR);
            probe[1]=h;probe[0]=1;
            C(cx_wait(dec(argv[2]),&status)==1);return 99;
        }
        if(eq(mode,"copy-owner")){
            h=spawn("copy",0);C(h!=CX_ERROR);
            probe[1]=h;probe[0]=1;
            C(cx_job_wait(h,&status)==1);return 99;
        }
        if(eq(mode,"copy")){
            probe[2]=(uintptr_t)cross;probe[0]=1;
            C(cx_read("test/immutable",14,0,cross,16)==16);
            C(cx_create("test/job-resumed",16)==0);
            return 99;
        }
        if(eq(mode,"integrity")){
            for(unsigned i=0;i<sizeof(cross);i++)cross[i]=(unsigned char)(i*13u+0x5a);
            probe[0]=1;
            for(;;){
                for(unsigned i=0;i<sizeof(cross);i++)C(((volatile unsigned char *)cross)[i]==(unsigned char)(i*13u+0x5a));
                probe[1]++;
            }
        }
        if(eq(mode,"allocation")){C(spawn("exit","37")==CX_ERROR);return 0;}
        if(eq(mode,"spin")){
            probe[0]=1;
            for(;;)probe[1]++;
        }
        if(eq(mode,"sleep")){probe[0]=1;C(cx_sleep(10000)==0);return 37;}
        if(eq(mode,"delay")){C(cx_sleep(3)==0);cx_exit(0x1234567887654321ull);}
        if(eq(mode,"exit"))cx_exit(argc==3?dec(argv[2]):37);
        if(eq(mode,"fault")){__asm__ volatile("ud2");return 99;}
        if(eq(mode,"deny")){C(argc==3);no_result(dec(argv[2]));return 0;}
        if(eq(mode,"branch")){
            h=spawn("spin",0);C(h!=CX_ERROR);
            C(cx_create(ready,sizeof(ready)-1)==0);
            C(cx_job_wait(h,&status)==1);return 99;
        }
        if((strlen(mode)>=6&&!memcmp(mode,"owner-",6))){
            h=spawn("branch",0);C(h!=CX_ERROR);
            ready_wait();
            uint64_t zombie=spawn("exit","91");C(zombie!=CX_ERROR);
            C(cx_sleep(5)==0);
            if(mode[6]=='e')return 41;
            if(mode[6]=='f'){__asm__ volatile("ud2");return 99;}
            if(mode[6]=='w'){C(cx_job_wait(h,&status)==1);return 99;}
            C(cx_sleep(10000)==0);return 99;
        }
        return 100;
    }
    no_result(0);no_result(UINT64_MAX);
    C(cx_job_spawn("missing",7,0,0)==CX_ERROR);
    C(cx_job_spawn(path,sizeof(path)-1,(const void *)1,1)==CX_ERROR);
    C(cx_job_spawn(path,sizeof(path)-1,0,33)==CX_ERROR);
    h=spawn("spin",0);C(h!=CX_ERROR);
    probe[0]=1;probe[1]=h;
    C(cx_sleep(5)==0);
    /* Kernel observer supplies the actual reusable slot without exposing it
     * through the production API. A live standalone test skips only this. */
    if(probe[2]){
        status=0xabc;
        C(cx_reap(probe[2],&status)==CX_ERROR&&status==0xabc);
        C(cx_wait(probe[2],&status)==CX_ERROR&&status==0xabc);
        probe[3]=1;
    }
    status=0xabc;
    C(cx_job_poll(h,&status)==0&&status==0xabc);
    C(cx_call(18,h,3,(uintptr_t)&status,0,0,0)==CX_ERROR&&status==0xabc);
    invalid_buffers(h);
    C(cx_job_poll(h,&status)==0&&status==0xabc);
    char handle[32];num(handle,h);
    uint64_t deny=spawn("deny",handle);C(deny!=CX_ERROR);
    C(cx_job_wait(deny,&status)==1&&status==0);no_result(deny);
    C(cx_job_stop(h,&status)==2&&status==UINT64_MAX);
    uint64_t stale=h;no_result(stale);probe[0]=2;
    for(unsigned i=0;i<24;i++){
        h=spawn(i&1?"sleep":"spin",0);C(h!=CX_ERROR&&h>stale);
        no_result(stale);
        C(cx_sleep(1)==0);
        C(cx_job_stop(h,&status)==2&&status==UINT64_MAX);
        stale=h;no_result(stale);
    }
    /* Valid unaligned result spanning two pages; immediate and blocking paths. */
    uint64_t *out=(void *)(cross+4092),got;
    h=spawn("delay",0);C(h!=CX_ERROR);
    C(cx_job_wait(h,out)==1);memcpy(&got,out,8);C(got==0x1234567887654321ull);
    no_result(h);
    const char *modes[]={"exit","fault"};
    for(unsigned i=0;i<2;i++){
        h=spawn(modes[i],"18446744073709551615");C(h!=CX_ERROR);
        C(cx_sleep(5)==0);invalid_buffers(h);
        C(cx_job_stop(h,out)==1);memcpy(&got,out,8);C(got==UINT64_MAX);no_result(h);
    }
    h=spawn("exit","1311768467139281697");C(h!=CX_ERROR);
    C(cx_sleep(5)==0);C(cx_job_poll(h,out)==1);
    memcpy(&got,out,8);C(got==1311768467139281697ull);no_result(h);
    /* Saturation and repeated reuse; no task slot may leak. */
    for(unsigned pass=0;pass<3;pass++){
        uint64_t handles[7];unsigned n=0;
        while(n<7){h=spawn("spin",0);if(h==CX_ERROR)break;handles[n++]=h;}
        C(n>=1&&n<7);
        for(unsigned i=0;i<n;i++)C(cx_job_stop(handles[i],&status)==2);
        h=spawn("exit","37");C(h!=CX_ERROR&&cx_job_wait(h,&status)==1&&status==37);
    }
    const char *owners[]={"owner-exit","owner-fault","owner-wait","owner-sleep"};
    for(unsigned i=0;i<4;i++){
        C(cx_size(ready,sizeof(ready)-1)==CX_ERROR);
        h=spawn(owners[i],0);C(h!=CX_ERROR);
        ready_wait();C(cx_sleep(10)==0);
        if(i<2)C(cx_job_wait(h,&status)==1&&status==(i?UINT64_MAX:41));
        else C(cx_job_stop(h,&status)==2&&status==UINT64_MAX);
        no_result(h);C(cx_remove(ready,sizeof(ready)-1)==0);
    }
    probe[0]=3;
    return 0;
}
