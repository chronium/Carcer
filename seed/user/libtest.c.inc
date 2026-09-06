#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#define CHECK(x) do {if(!(x))cx_exit(__LINE__);}while(0)
int main(void) {
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
    return 0;
}
