#include "cxlib.h"
struct cx_stream { char path[256]; uint64_t pos; unsigned read,write,append,eof,error,owned; };
static FILE in_stream,out_stream,err_stream;
FILE *stdin=&in_stream,*stdout=&out_stream,*stderr=&err_stream;
static size_t pathlen(const char *p){if(!p)return 0;size_t n=0;while(n<256&&p[n])++n;return n&&n<256?n:0;}
FILE *fopen(const char *p,const char *mode) {
    size_t n=pathlen(p);
    if(!n||!mode||!strchr("rwa",mode[0])||!mode[0]){errno=EINVAL;return NULL;}
    int plus=0,binary=0,exclusive=0;
    for(const char *m=mode+1;*m;m++) {
        if(*m=='+'&&!plus)plus=1;
        else if(*m=='b'&&!binary)binary=1;
        else if(*m=='x'&&!exclusive&&mode[0]=='w')exclusive=1;
        else {errno=EINVAL;return NULL;}
    }
    FILE *f=calloc(1,sizeof(*f));if(!f)return NULL;
    strcpy(f->path,p);f->owned=1;f->read=mode[0]=='r'||plus;f->write=mode[0]!='r'||plus;f->append=mode[0]=='a';
    uint64_t size=cx_size(p,n),attr=cx_attributes(p,n);
    if(f->write && attr!=CX_ERROR && (attr&1)){errno=EACCES;goto fail;}
    if(exclusive) {
        if(cx_create(p,n)!=0){errno=size==CX_ERROR?EIO:EEXIST;goto fail;}
        size=0;
    } else if(size==CX_ERROR) {
        if(mode[0]=='r'){errno=ENOENT;goto fail;}
        if(cx_write(p,n,0,NULL,0)!=0){errno=EIO;goto fail;}size=0;
    }
    if(mode[0]=='w' && size && cx_truncate(p,n,0)!=0){errno=EIO;goto fail;}
    if(f->append)f->pos=size;
    return f;
fail:
    free(f);return NULL;
}
int fclose(FILE *f){if(!f){errno=EINVAL;return EOF;}int r=f->error?EOF:0;if(f->owned)free(f);else memset(f,0,sizeof(*f));return r;}
int fflush(FILE *f){return f&&f->error?EOF:0;}
size_t fread(void *p,size_t size,size_t count,FILE *f) {
    if(!size||!count)return 0;
    if(!f||!f->read||!p||count>SIZE_MAX/size){errno=EINVAL;if(f)f->error=1;return 0;}
    uint64_t z=cx_size(f->path,strlen(f->path));
    if(z==CX_ERROR){f->error=1;errno=EIO;return 0;}
    size_t want=size*count;
    if(f->pos>=z){f->eof=1;return 0;}
    uint64_t n=cx_read(f->path,strlen(f->path),f->pos,p,want);
    if(n==CX_ERROR){f->error=1;errno=EIO;return 0;}
    f->pos+=n;if(n<want)f->eof=1;
    return n/size;
}
size_t fwrite(const void *p,size_t size,size_t count,FILE *f) {
    if(!size||!count)return 0;
    if(!f||!f->write||!p||count>SIZE_MAX/size){errno=EINVAL;if(f)f->error=1;return 0;}
    size_t n=size*count,pn=strlen(f->path);
    uint64_t z=cx_size(f->path,pn);
    if(z==CX_ERROR)goto fail;
    if(f->append)f->pos=z;
    if(f->pos>UINT32_MAX || n>UINT32_MAX-f->pos)goto fail;
    if(f->pos>z && cx_truncate(f->path,pn,(uint32_t)f->pos)!=0)goto fail;
    uint64_t written=cx_write(f->path,pn,f->pos,p,n);
    if(written==CX_ERROR)goto fail;
    f->pos+=written;
    if(written!=n){f->error=1;errno=EIO;}
    return written/size;
fail:
    f->error=1;errno=EIO;return 0;
}
int fseek(FILE *f,long offset,int whence) {
    if(!f || !f->path[0]){errno=EINVAL;return -1;}
    uint64_t base;
    if(whence==SEEK_SET)base=0;
    else if(whence==SEEK_CUR)base=f->pos;
    else if(whence==SEEK_END){base=cx_size(f->path,strlen(f->path));if(base==CX_ERROR){errno=EIO;return -1;}}
    else {errno=EINVAL;return -1;}
    uint64_t magnitude=offset<0?0ull-(uint64_t)offset:(uint64_t)offset;
    if((offset<0 && magnitude>base)||(offset>=0 && magnitude>UINT32_MAX-base)){errno=EINVAL;return -1;}
    f->pos=offset<0?base-magnitude:base+magnitude;f->eof=0;return 0;
}
long ftell(FILE *f){if(!f){errno=EINVAL;return -1;}return (long)f->pos;}
void rewind(FILE *f){if(!fseek(f,0,SEEK_SET))clearerr(f);}
int fgetc(FILE *f){unsigned char c;return fread(&c,1,1,f)==1?c:EOF;}
int fputc(int c,FILE *f){unsigned char b=(unsigned char)c;return fwrite(&b,1,1,f)==1?b:EOF;}
char *fgets(char *p,int n,FILE *f){if(!p||n<=0){errno=EINVAL;return NULL;}int i=0,c=0;while(i<n-1&&(c=fgetc(f))!=EOF){p[i++]=(char)c;if(c=='\n')break;}p[i]=0;return i||n==1?p:NULL;}
int fputs(const char *s,FILE *f){size_t n=strlen(s);return fwrite(s,1,n,f)==n?0:EOF;}
int puts(const char *s){return fputs(s,stdout)==EOF||fputc('\n',stdout)==EOF?EOF:0;}
int putchar(int c){return fputc(c,stdout);}
int feof(FILE *f){return f?f->eof:0;}
int ferror(FILE *f){return f?f->error:1;}
void clearerr(FILE *f){if(f)f->error=f->eof=0;}
int remove(const char *p){size_t n=pathlen(p);if(n&&cx_remove(p,n)==0)return 0;errno=EIO;return -1;}
int rename(const char *p,const char *q){size_t n=pathlen(p),m=pathlen(q);if(n&&m&&cx_rename(p,n,q,m)==0)return 0;errno=EIO;return -1;}
int cx_stdio(const char *out,const char *err) {
    FILE *o=NULL,*e=NULL;
    if(out && !(o=fopen(out,"a")))return -1;
    if(err && !(e=fopen(err,"a"))){if(o)fclose(o);return -1;}
    if(o){if(stdout->owned)fclose(stdout);stdout=o;}
    if(e){if(stderr->owned)fclose(stderr);stderr=e;}
    return 0;
}
