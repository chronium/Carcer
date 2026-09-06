#include "cx.h"
/* Boot observer makes the middle .probe page read-only before scheduling. */
__attribute__((section(".probe"),aligned(4096))) unsigned char pages[12288]={[4096]=0x5a};
#define C(x) do{if(!(x))cx_exit(__LINE__);}while(0)
static int filled(const void *p,size_t n,unsigned char c){const unsigned char *s=p;for(size_t i=0;i<n;i++)if(s[i]!=c)return 0;return 1;}
static int compare(const unsigned char *a,size_t n,const unsigned char *b,size_t m) {
    size_t z=n<m?n:m;int r=memcmp(a,b,z);return r?r:n<m?-1:n>m;
}
int main(void) {
    uint64_t count=cx_files((void *)UINT64_MAX,0);C(count>0&&count<128);
    unsigned char *p=cx_alloc(128*272+8192);C(p);
    struct cx_file_record *r=(void *)(p+4096-8);
    memset(r,0xa5,128*272);
    C(cx_files(r,count-1)==CX_ERROR&&filled(r,128*272,0xa5));
    C(cx_files(r,129)==CX_ERROR&&cx_files(r,UINT64_MAX)==CX_ERROR);
    C(cx_files(r,1ull<<63)==CX_ERROR&&filled(r,128*272,0xa5));
    C(cx_files((void *)1,128)==CX_ERROR);
    C(cx_files(r,128)==count);
    int sealed=0;
    for(size_t i=0;i<count;i++){
        C(r[i].length>0&&r[i].length<=255&&r[i].reserved==0&&r[i].reserved2==0);
        C(filled(r[i].path+r[i].length,256-r[i].length,0));
        C(cx_size((const char *)r[i].path,r[i].length)==r[i].size);
        C(cx_attributes((const char *)r[i].path,r[i].length)==r[i].attributes);
        if(r[i].attributes&1)sealed=1;
        if(i)C(compare(r[i-1].path,r[i-1].length,r[i].path,r[i].length)<0);
    }
    C(sealed&&filled(r+count,(128-count)*272,0xa5));
    unsigned char *end=(void *)(uintptr_t)cx_brk(0);
    memset(end-8,0x6b,8);
    C(cx_files((void *)(end-8),128)==CX_ERROR&&filled(end-8,8,0x6b));
    /* Cross-page failure must leave the entire valid prefix untouched. */
    memset(pages,0x7c,4096);
    C(cx_files((void *)pages,128)==CX_ERROR&&filled(pages,4096,0x7c)&&pages[4096]==0x5a);
    char names[128][16];
    unsigned made=0;
    while(count+made<128){
        memcpy(names[made],"test/enum-",10);
        names[made][10]='0'+made/100;
        names[made][11]='0'+(made/10)%10;
        names[made][12]='0'+made%10;
        names[made][13]=0;
        C(cx_create(names[made],13)==0);made++;
    }
    C(cx_files(r,128)==128&&cx_files(0,0)==128);
    char longname[255];memset(longname,'q',255);
    longname[0]='z';longname[1]=0; /* Names are spans, including embedded NUL. */
    longname[253]=(char)0xc3;longname[254]=(char)0xa9;
    C(cx_rename(names[0],13,longname,255)==0);
    C(cx_files(r,128)==128);
    int found=0;
    for(size_t i=0;i<128;i++)if(r[i].length==255&&!memcmp(r[i].path,longname,255)){found++;C(r[i].path[255]==0);}
    C(found==1&&cx_remove(longname,255)==0);
    for(unsigned i=1;i<made;i++)C(cx_remove(names[i],13)==0);
    C(cx_files(r,128)==count);
    return 0;
}
