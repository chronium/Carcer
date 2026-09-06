#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#define CHECK(x) do {if(!(x))cx_exit(__LINE__);}while(0)

#define A "test/rt-a"
#define B "test/rt-b"
#define SELF "seed/user/libtest.cxe"
static uint64_t openp(const char *p,uint64_t flags){return cx_open(p,strlen(p),flags);}
static uint64_t job(const char *mode,const char *extra) {
    struct cx_arg args[3]={{SELF,sizeof(SELF)-1},{mode,strlen(mode)},{extra,extra?strlen(extra):0}};
    return cx_job_spawn(SELF,sizeof(SELF)-1,args,extra?3:2);
}
static void collect(uint64_t h) {
    uint64_t status=~0ull;
    CHECK(h!=CX_ERROR&&cx_job_wait(h,&status)==1&&status==0);
}
static void number(char *p,uint64_t x) {
    char b[21];unsigned n=0;do{b[n++]=(char)('0'+x%10);x/=10;}while(x);
    for(unsigned i=0;i<n;i++)p[i]=b[n-i-1];
    p[n]=0;
}
static uint64_t decimal(const char *p) {
    uint64_t x=0;while(*p){CHECK(*p>='0'&&*p<='9');x=x*10+(*p++-'0');}return x;
}
static int file_mode(int argc,char **argv) {
    const char *mode=argv[1];
    if(!strcmp(mode,"open-fail")) {
        CHECK(openp(A,3|CX_OPEN_TRUNC)==CX_ERROR);
        CHECK(openp(B,7)==CX_ERROR);
        CHECK(cx_size(A,sizeof(A)-1)==4&&cx_size(B,sizeof(B)-1)==CX_ERROR);
        char b[4];CHECK(cx_read(A,sizeof(A)-1,0,b,4)==4&&!memcmp(b,"keep",4));
        return 0;
    }
    if(!strcmp(mode,"foreign")) {
        CHECK(argc==3);uint64_t h=decimal(argv[2]);
        char c=77;struct cx_file_info info;
        CHECK(cx_close(h)==CX_ERROR&&cx_fread(h,&c,1)==CX_ERROR&&c==77);
        CHECK(cx_fwrite(h,"X",1)==CX_ERROR&&cx_fseek(h,0,0)==CX_ERROR);
        CHECK(cx_ftruncate(h,0)==CX_ERROR&&cx_fstat(h,&info)==CX_ERROR);
        return 0;
    }
    if(!strcmp(mode,"writer")) {
        CHECK(argc==3);unsigned who=(unsigned)decimal(argv[2]);CHECK(who<2);
        uint64_t h=openp(A,CX_OPEN_WRITE|CX_OPEN_APPEND);CHECK(h!=CX_ERROR);
        while(cx_size(B,sizeof(B)-1)!=1)cx_sleep(1);
        unsigned char record[64];
        for(unsigned i=0;i<256;i++) {
            record[0]=(unsigned char)who;record[1]=(unsigned char)i;
            for(unsigned j=2;j<64;j++)record[j]=(unsigned char)(who*53+i*7+j);
            CHECK(cx_fwrite(h,record,64)==64);
        }
        CHECK(cx_close(h)==0);return 0;
    }
    if(!strncmp(mode,"leak-",5)) {
        uint64_t hs[16];
        CHECK(cx_create(A,sizeof(A)-1)==0);
        for(unsigned i=0;i<16;i++){hs[i]=openp(A,3);CHECK(hs[i]!=CX_ERROR);}
        unsigned char bytes[4096];memset(bytes,0x6d,sizeof(bytes));
        CHECK(cx_fwrite(hs[0],bytes,sizeof(bytes))==sizeof(bytes));
        CHECK(cx_remove(A,sizeof(A)-1)==0);
        if(!strcmp(mode,"leak-exit"))return 0;
        if(!strcmp(mode,"leak-fault"))__asm__ volatile("ud2");
        if(!strcmp(mode,"leak-owner")) {
            uint64_t child=job("leak-spin",NULL),status;
            CHECK(child!=CX_ERROR);
            (void)cx_job_wait(child,&status);return 99;
        }
        if(!strcmp(mode,"leak-sleep")){cx_sleep(100000);return 99;}
        for(;;)__asm__ volatile("pause");
    }

    if(!strcmp(mode,"namespace-move")) {
        CHECK(cx_create(A,sizeof(A)-1)==0);
        CHECK(cx_write(A,sizeof(A)-1,0,"move",4)==4);
        CHECK(cx_rename(A,sizeof(A)-1,"0/fh-move",9)==0);
        return 0;
    }
    if(!strcmp(mode,"copy-resume")) {
        uint64_t h=openp("test/immutable",CX_OPEN_READ);
        unsigned char *p=cx_alloc(32u*1024u*1024u);
        CHECK(h!=CX_ERROR&&p);
        memset(p,0x79,32u*1024u*1024u);
        CHECK(cx_fseek(h,2,0)==2);
        uint64_t count=cx_fread(h,p,32u*1024u*1024u);
        CHECK(count==32u*1024u*1024u-2);
        CHECK(!memcmp(p,"aled",4));
        for(uint64_t i=4;i<count;i++)CHECK(p[i]==0);
        CHECK(p[count]==0x79&&p[count+1]==0x79);
        struct cx_file_info info;
        CHECK(cx_fstat(h,&info)==24&&info.position==32u*1024u*1024u&&
              info.size==info.position&&(info.attributes&1));
        CHECK(cx_fread(h,(void *)1,1)==0);
        CHECK(cx_fseek(h,0,0)==0&&cx_fread(h,p,6)==6&&!memcmp(p,"sealed",6));
        CHECK(cx_fseek(h,0,1)==6&&cx_close(h)==0);
        return 0;
    }
    if(!strcmp(mode,"copy-handle")) {
        uint64_t h=openp("test/immutable",CX_OPEN_READ);
        void *p=cx_alloc(32u*1024u*1024u);
        CHECK(h!=CX_ERROR&&p);
        CHECK(cx_fread(h,p,32u*1024u*1024u)==32u*1024u*1024u);
        CHECK(cx_write(B,sizeof(B)-1,0,"BAD",3)==3);
        return 99;
    }
    return 98;
}
static void stable_files(void) {
    const unsigned badflags[]={0,64,5,9,10,17,33};
    for(unsigned i=0;i<sizeof(badflags)/sizeof(*badflags);i++)
        CHECK(openp(A,badflags[i])==CX_ERROR&&cx_size(A,sizeof(A)-1)==CX_ERROR);
    CHECK(cx_open((const char *)1,5,7)==CX_ERROR);
    CHECK(cx_open(A,0,7)==CX_ERROR);
    uint64_t a=openp(A,3|CX_OPEN_CREATE|CX_OPEN_EXCL);CHECK(a!=CX_ERROR);
    CHECK(cx_fwrite(a,"original",8)==8);
    uint64_t same=openp(A,CX_OPEN_READ);CHECK(same!=CX_ERROR);
    uint64_t b=openp(B,3|CX_OPEN_CREATE|CX_OPEN_EXCL);CHECK(b!=CX_ERROR);
    CHECK(cx_fwrite(b,"replaced",8)==8);
    CHECK(cx_rename(A,sizeof(A)-1,B,sizeof(B)-1)==0);
    CHECK(cx_size(A,sizeof(A)-1)==CX_ERROR);
    CHECK(cx_fseek(b,0,0)==0);char bytes[80];
    CHECK(cx_fread(b,bytes,8)==8&&!memcmp(bytes,"replaced",8));
    CHECK(cx_fseek(a,0,0)==0&&cx_fwrite(a,"O",1)==1);
    CHECK(cx_fread(same,bytes,8)==8&&!memcmp(bytes,"Original",8));
    CHECK(cx_remove(B,sizeof(B)-1)==0);
    uint64_t newfile=openp(B,3|CX_OPEN_CREATE);CHECK(newfile!=CX_ERROR);
    CHECK(cx_fwrite(newfile,"new",3)==3);
    CHECK(cx_fseek(a,0,0)==0&&cx_fread(a,bytes,8)==8&&!memcmp(bytes,"Original",8));
    CHECK(cx_fseek(b,0,0)==0&&cx_fread(b,bytes,8)==8&&!memcmp(bytes,"replaced",8));
    CHECK(cx_fwrite(same,"!",1)==CX_ERROR&&cx_ftruncate(same,0)==CX_ERROR);
    CHECK(cx_ftruncate(a,3)==0&&cx_fseek(same,0,2)==3);
    CHECK(cx_fseek(a,8,0)==8&&cx_fwrite(a,"!",1)==1);
    CHECK(cx_fseek(a,0,0)==0&&cx_fread(a,bytes,10)==9&&!memcmp(bytes,"Ori\0\0\0\0\0!",9));
    CHECK(cx_fseek(a,20,0)==20&&cx_fread(a,(void *)1,3)==0);
    CHECK(cx_fseek(a,0,1)==20);
    CHECK(cx_fwrite(a,(void *)1,0)==0&&cx_fseek(a,0,2)==9);
    CHECK(cx_fseek(a,INT64_MIN,1)==CX_ERROR&&cx_fseek(a,0,1)==9);
    CHECK(cx_fseek(a,UINT32_MAX,0)==UINT32_MAX);
    CHECK(cx_fwrite(a,"x",1)==CX_ERROR);
    CHECK(cx_fseek(a,1,1)==CX_ERROR&&cx_fseek(a,0,1)==UINT32_MAX);
    CHECK(cx_ftruncate(a,UINT64_MAX)==CX_ERROR);
    struct cx_file_info info;CHECK(cx_fstat(a,&info)==24&&info.size==9&&info.position==UINT32_MAX&&!info.attributes);
    CHECK(cx_call(20,a,6,0,0,0,0)==CX_ERROR);
    CHECK(cx_close(0)==CX_ERROR&&cx_close(CX_ERROR)==CX_ERROR);
    char token[21];number(token,a);collect(job("foreign",token));
    CHECK(cx_fseek(a,0,0)==0);
    /* This raw SDK allocation is independent of libc's blocks. The final
     * four bytes precede unmapped brk memory; no allocation occurs meanwhile. */
    unsigned char *raw=cx_alloc(8192);CHECK(raw);
    uintptr_t end=(uintptr_t)cx_brk(0);
    unsigned char *edge=(unsigned char *)(end-4);
    memset(edge,0x71,4);
    CHECK(cx_fread(a,edge,8)==CX_ERROR&&cx_fseek(a,0,1)==0);
    for(unsigned i=0;i<4;i++)CHECK(edge[i]==0x71);
    CHECK(cx_call(20,a,4,end-4,24,0,0)==CX_ERROR);
    for(unsigned i=0;i<4;i++)CHECK(edge[i]==0x71);
    CHECK(cx_fwrite(a,edge,8)==CX_ERROR&&cx_fseek(a,0,1)==0);
    CHECK(cx_fread(a,(void *)(uintptr_t)SELF,8)==CX_ERROR);
    unsigned char *cross=(unsigned char *)(((uintptr_t)raw+4095)&~4095ull)+4093;
    CHECK((uintptr_t)cross+24<=end);
    CHECK(cx_call(20,a,4,(uintptr_t)cross,24,0,0)==24);
    CHECK(cx_fseek(a,0,0)==0&&cx_fread(a,cross,9)==9&&!memcmp(cross,"Ori\0\0\0\0\0!",9));
    CHECK(cx_fseek(a,0,0)==0&&cx_fwrite(a,cross,9)==9);
    CHECK(cx_close(a)==0&&cx_close(a)==CX_ERROR);
    CHECK(cx_close(same)==0&&cx_close(b)==0&&cx_close(newfile)==0);
    uint64_t hs[16];
    for(unsigned i=0;i<16;i++){hs[i]=openp(B,3);CHECK(hs[i]!=CX_ERROR&&hs[i]!=a);}
    CHECK(openp(B,3|CX_OPEN_TRUNC)==CX_ERROR&&cx_size(B,sizeof(B)-1)==3);
    CHECK(openp(A,7)==CX_ERROR&&cx_size(A,sizeof(A)-1)==CX_ERROR);
    CHECK(cx_fread(a,bytes,1)==CX_ERROR);
    for(unsigned i=0;i<16;i++)CHECK(cx_close(hs[i])==0);
    for(unsigned i=0;i<32;i++){uint64_t q=openp(B,1);CHECK(q!=CX_ERROR&&q!=a&&cx_close(q)==0);}
    uint64_t ro=openp("test/immutable",1);CHECK(ro!=CX_ERROR);
    CHECK(openp("test/immutable",3)==CX_ERROR&&cx_ftruncate(ro,0)==CX_ERROR);
    CHECK(cx_fstat(ro,&info)==24&&(info.attributes&1)&&cx_close(ro)==0);
    CHECK(cx_remove(B,sizeof(B)-1)==0);
    /* C streams retain both source and replaced target objects. */
    FILE *fa=fopen(A,"w+"),*fb=fopen(B,"w+");CHECK(fa&&fb);
    CHECK(fputs("left",fa)==0&&fputs("right",fb)==0);
    CHECK(rename(A,B)==0&&remove(B)==0);
    FILE *fresh=fopen(B,"wx");CHECK(fresh&&fputs("new",fresh)==0&&fclose(fresh)==0);
    rewind(fa);rewind(fb);
    CHECK(fread(bytes,1,4,fa)==4&&!memcmp(bytes,"left",4));
    CHECK(fread(bytes,1,5,fb)==5&&!memcmp(bytes,"right",5));
    CHECK(fclose(fa)==0&&fclose(fb)==0&&remove(B)==0);
    /* Two separately runnable writers compete on the same append target. */
    CHECK(cx_create(A,sizeof(A)-1)==0&&cx_create(B,sizeof(B)-1)==0);
    uint64_t first=job("writer","0"),second=job("writer","1");
    CHECK(first!=CX_ERROR&&second!=CX_ERROR);
    CHECK(cx_write(B,sizeof(B)-1,0,"G",1)==1);
    collect(first);collect(second);
    CHECK(cx_size(A,sizeof(A)-1)==512*64);
    uint64_t log=openp(A,1);CHECK(log!=CX_ERROR);
    unsigned char seen[2][256];memset(seen,0,sizeof(seen));
    for(unsigned i=0;i<512;i++) {
        CHECK(cx_fread(log,bytes,64)==64);
        unsigned who=(unsigned char)bytes[0],seq=(unsigned char)bytes[1];
        CHECK(who<2&&!seen[who][seq]);seen[who][seq]=1;
        for(unsigned j=2;j<64;j++)CHECK((unsigned char)bytes[j]==(unsigned char)(who*53+seq*7+j));
    }
    CHECK(cx_close(log)==0&&cx_remove(A,sizeof(A)-1)==0&&cx_remove(B,sizeof(B)-1)==0);
}

int main(int argc,char **argv) {
    if(argc>1)return file_mode(argc,argv);
    void *p[64];
    for(unsigned i=0;i<64;i++){p[i]=malloc(i*97+1);CHECK(p[i] && !((uintptr_t)p[i]&15));memset(p[i],(int)i,i*97+1);}
    for(unsigned i=0;i<64;i+=2)free(p[i]);
    for(unsigned i=1;i<64;i+=2){unsigned char *q=p[i];for(unsigned j=0;j<i*97+1;j++)CHECK(q[j]==i);free(q);}
    uint64_t high=cx_brk(0);
    for(unsigned cycle=0;cycle<10;cycle++){void *q=malloc(120000);CHECK(q);free(q);}
    uint64_t after=cx_brk(0);CHECK(after<=high+131072);
    p[0]=calloc(17,19);CHECK(p[0]);for(unsigned i=0;i<323;i++)CHECK(!((char *)p[0])[i]);
    memset(p[0],0xab,323);p[1]=realloc(p[0],80000);CHECK(p[1]);for(unsigned i=0;i<323;i++)CHECK(((unsigned char *)p[1])[i]==0xab);free(p[1]);
    CHECK(!calloc(SIZE_MAX,2));CHECK(!malloc(SIZE_MAX));
    char b[128],*end;
    CHECK(snprintf(b,sizeof(b),"%#08x %lld %.3s %.2f",0x2a,(-9223372036854775807LL-1),"abcdef",1.25)==38);
    CHECK(!strcmp(b,"0x00002a -9223372036854775808 abc 1.25"));
    CHECK(snprintf(b,5,"%s","abcdefgh")==8&&!strcmp(b,"abcd"));
    CHECK(snprintf(NULL,0,"%08u",12)==8);
    CHECK(snprintf(b,sizeof(b),"%*.*d",-8,4,12)==8&&!strcmp(b,"0012    "));
    CHECK(snprintf(b,sizeof(b),"%.2147483648s","abc")==-1);
    CHECK(snprintf(b,sizeof(b),"%2147483648s","abc")==-1);
    CHECK(snprintf(b,sizeof(b),"%.*s",-1,"abc")==3&&!strcmp(b,"abc"));
    CHECK(snprintf(b,sizeof(b),"%.0s",(char *)(uintptr_t)1)==0&&!b[0]);
    CHECK(strtod("0e999",&end)==0&&!*end);
    CHECK(strtod("123456789012345678901234567890e-20",&end)>1234567890.0&&!*end);
    CHECK(strtol(" -0x7f!",&end,0)==-127&&*end=='!');
    errno=0;CHECK(strtoul("18446744073709551616",&end,10)==ULONG_MAX&&errno==ERANGE&&!*end);
    errno=0;CHECK(strtol("-9223372036854775808",&end,10)==LONG_MIN&&!errno);
    int x=0,n=0;unsigned y=0;float f=0;char word[20];
    CHECK(sscanf(" -12 0x2a 1.25 hello world","%d %x %f %19[^\n]%n",&x,&y,&f,word,&n)==4);
    CHECK(x==-12&&y==42&&f==1.25f&&!strcmp(word,"hello world")&&n==26);
    CHECK(sscanf("abcd","%2c%n",word,&n)==1&&n==2&&word[0]=='a'&&word[1]=='b');
    CHECK(sscanf("","%d",&x)==EOF);
    CHECK(sscanf("","")==0 && sscanf("abc","abc")==0);
    CHECK(sscanf("xyz","abc")==0 && sscanf("abc","abcd")==EOF);
    CHECK(sscanf(""," ")==0 && sscanf("","%n",&n)==0 && n==0);

    char ch='Z';f=7.5f;x=321;y=654;
    CHECK(sscanf("1e+X","%f%c",&f,&ch)==0&&f==7.5f&&ch=='Z');
    CHECK(sscanf("1e","%f",&f)==0&&f==7.5f);
    CHECK(sscanf("1e3","%2f%c",&f,&ch)==0&&f==7.5f&&ch=='Z');
    CHECK(sscanf("1e+3","%3f",&f)==0&&f==7.5f);
    CHECK(sscanf("+1","%1f",&f)==0&&f==7.5f);
    CHECK(sscanf(".","%f",&f)==0&&f==7.5f);
    CHECK(sscanf("0xG","%i%c",&x,&ch)==0&&x==321&&ch=='Z');
    CHECK(sscanf("0x1","%2x",&y)==0&&y==654);
    CHECK(sscanf("0x","%i",&x)==0&&x==321);
    CHECK(sscanf("-","%u",&y)==0&&y==654);
    CHECK(sscanf("1e+3!","%4f%c",&f,&ch)==2&&f==1000&&ch=='!');
    CHECK(sscanf("0x1X","%3i%c",&x,&ch)==2&&x==1&&ch=='X');
    CHECK(sscanf("0779","%i%c",&x,&ch)==2&&x==63&&ch=='9');
    CHECK(sscanf("2 1e+","%d%f",&x,&f)==1&&x==2&&f==1000);
    CHECK(sscanf("1e+","%*f%d",&x)==0&&x==2);
    CHECK(cx_create("test/rt-a",9)==0);
    CHECK(cx_create("test/rt-a",9)==CX_ERROR);
    CHECK(cx_write("test/rt-a",9,0,"old contents",12)==12);
    FILE *a=fopen("test/rt-a","w+");CHECK(a&&cx_size("test/rt-a",9)==0);
    CHECK(fwrite("abc",1,3,a)==3&&fseek(a,8,SEEK_SET)==0);
    CHECK(fputc('Z',a)=='Z'&&cx_size("test/rt-a",9)==9);
    rewind(a);memset(b,1,sizeof(b));CHECK(fread(b,2,5,a)==4&&ftell(a)==9&&feof(a));
    CHECK(!memcmp(b,"abc\0\0\0\0\0Z",9));CHECK(fseek(a,-1,SEEK_END)==0&&!feof(a)&&fgetc(a)=='Z');
    CHECK(fseek(a,LONG_MIN,SEEK_CUR)==-1&&ftell(a)==9);
    CHECK(fclose(a)==0);
    a=fopen("test/rt-a","a");CHECK(a&&fseek(a,0,SEEK_SET)==0&&fputs("!",a)==0&&fclose(a)==0);
    CHECK(cx_size("test/rt-a",9)==10);
    CHECK(!fopen("test/rt-a","wx"));CHECK(cx_write("test/rt-b",9,0,"replace",7)==7);
    CHECK(rename("test/rt-a","test/rt-b")==0&&cx_size("test/rt-a",9)==CX_ERROR&&cx_size("test/rt-b",9)==10);
    CHECK(rename("test/rt-b","test/rt-b")==0);
    CHECK(cx_call(14,(uintptr_t)"test/rt-b",9,3,1,8,0)==CX_ERROR);
    CHECK(cx_call(14,(uintptr_t)"test/rt-b",9,1,UINT64_MAX,0,0)==CX_ERROR);
    CHECK(cx_call(14,(uintptr_t)"test/rt-b",9,4,0,0,0)==CX_ERROR);
    CHECK(cx_size("test/rt-b",9)==10);
    CHECK(!fopen("test/immutable","w")&&remove("test/immutable")==-1);
    CHECK(rename("test/rt-b","test/immutable")==-1&&rename("test/immutable","test/rt-b")==-1);
    CHECK(cx_size("test/rt-b",9)==10);
    CHECK(remove("test/rt-b")==0&&remove("test/rt-b")==-1);
    stable_files();
    return 0;
}
