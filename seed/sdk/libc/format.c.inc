#include "cxlib.h"
struct output { char *data;size_t cap,count;int error; };
static void emit(struct output *o,char c) {
    if(o->count>=INT_MAX){o->error=1;return;}
    if(o->cap && o->count<o->cap-1)o->data[o->count]=c;
    ++o->count;
}
static void repeat(struct output *o,char c,size_t n){while(n--&&!o->error)emit(o,c);}
static int decimal(const char **p) {
    int n=0;
    while(isdigit((unsigned char)**p)){
        int d=*(*p)++-'0';
        if(n>(INT_MAX-d)/10)return -1;
        n=n*10+d;
    }
    return n;
}
int vsnprintf(char *buf,size_t cap,const char *fmt,va_list input) {
    struct output o={buf,cap,0,0};
    va_list ap;va_copy(ap,input);
    if((cap&&!buf)||!fmt){va_end(ap);errno=EINVAL;return -1;}
    while(*fmt && !o.error) {
        if(*fmt!='%'){emit(&o,*fmt++);continue;}
        ++fmt;
        int left=0,plus=0,space=0,alt=0,zero=0;
        for(;;){if(*fmt=='-')left=1;else if(*fmt=='+')plus=1;else if(*fmt==' ')space=1;else if(*fmt=='#')alt=1;else if(*fmt=='0')zero=1;else break;++fmt;}
        int width=0,precision=-1;
        if(*fmt=='*'){++fmt;width=va_arg(ap,int);if(width<0){if(width==INT_MIN){o.error=1;break;}left=1;width=-width;}}
        else width=decimal(&fmt);
        if(*fmt=='.'){++fmt;if(*fmt=='*'){++fmt;precision=va_arg(ap,int);if(precision<0)precision=-1;}else {precision=decimal(&fmt);if(precision<0){o.error=1;break;}}}
        if(width<0){o.error=1;break;}
        int length=0;
        if(*fmt=='h'){++fmt;length=-1;if(*fmt=='h'){++fmt;length=-2;}}
        else if(*fmt=='l'){++fmt;length=1;if(*fmt=='l'){++fmt;length=2;}}
        else if(*fmt=='z'){++fmt;length=3;}
        else if(*fmt=='t'){++fmt;length=4;}
        else if(*fmt=='j'){++fmt;length=2;}
        char spec=*fmt;
        if(!spec){o.error=1;break;}++fmt;
        char data[128],prefix[3];size_t n=0,pn=0,zeros=0;const char *s=data;
        int numeric=0;
        if(spec=='s') {
            if(length){o.error=1;break;}
            s=va_arg(ap,const char *);if(!s)s="(null)";
            while((precision<0||n<(size_t)precision)&&s[n])++n;
            zero=0;
        } else if(spec=='c'||spec=='%') {
            if(length){o.error=1;break;}
            data[n++]=spec=='%'?'%':(char)va_arg(ap,int);zero=0;
        } else if(strchr("diuoxXp",spec)) {
            numeric=1;
            uint64_t v=0;unsigned base=spec=='o'?8:(spec=='x'||spec=='X'||spec=='p')?16:10;
            int negative=0;
            if(spec=='p'){v=(uintptr_t)va_arg(ap,void *);alt=1;}
            else if(spec=='d'||spec=='i'){
                int64_t x;
                if(length==2)x=va_arg(ap,long long);
                else if(length>0)x=va_arg(ap,long);
                else {x=va_arg(ap,int);if(length==-1)x=(short)x;if(length==-2)x=(signed char)x;}
                negative=x<0;v=negative?0ull-(uint64_t)x:(uint64_t)x;
                if(negative)prefix[pn++]='-';else if(plus)prefix[pn++]='+';else if(space)prefix[pn++]=' ';
            } else {
                if(length==2)v=va_arg(ap,unsigned long long);
                else if(length>0)v=va_arg(ap,unsigned long);
                else {v=va_arg(ap,unsigned);if(length==-1)v=(unsigned short)v;if(length==-2)v=(unsigned char)v;}
            }
            uint64_t original=v;
            const char *digits=spec=='X'?"0123456789ABCDEF":"0123456789abcdef";
            if(v||precision!=0)do{data[n++]=digits[v%base];v/=base;}while(v);
            for(size_t i=0;i<n/2;i++){char c=data[i];data[i]=data[n-1-i];data[n-1-i]=c;}
            if(precision>0 && (size_t)precision>n)zeros=(size_t)precision-n;
            if(alt && base==8 && (!n||data[0]!='0') && !zeros)zeros=1;
            if(alt && base==16 && (original||spec=='p')){prefix[pn++]='0';prefix[pn++]=spec=='X'?'X':'x';}
            if(precision>=0)zero=0;
        } else if(spec=='f'||spec=='F') {
            if(length && length!=1){o.error=1;break;}
            double v=va_arg(ap,double);
            union {double d;uint64_t u;} bits={v};
            if(bits.u>>63){prefix[pn++]='-';v=-v;}
            else if(plus)prefix[pn++]='+';else if(space)prefix[pn++]=' ';
            if((bits.u&0x7ff0000000000000ull)==0x7ff0000000000000ull){
                s=(bits.u&0xfffffffffffffull)?"nan":"inf";n=3;zero=0;
            } else {
                if(precision<0)precision=6;
                if(precision>9 || v>1e18){o.error=1;break;}
                uint64_t scale=1;for(int i=0;i<precision;i++)scale*=10;
                uint64_t whole=(uint64_t)v;
                uint64_t frac=(uint64_t)((v-(double)whole)*(double)scale+.5);
                if(frac>=scale){++whole;frac=0;}
                do{data[n++]=(char)('0'+whole%10);whole/=10;}while(whole);
                for(size_t i=0;i<n/2;i++){char c=data[i];data[i]=data[n-1-i];data[n-1-i]=c;}
                if(precision||alt)data[n++]='.';
                size_t end=n+(size_t)precision;
                for(size_t i=end;i>n;){data[--i]=(char)('0'+frac%10);frac/=10;}n=end;
            }
        } else {o.error=1;break;}
        size_t total=n+pn+zeros,pad=(size_t)width>total?(size_t)width-total:0;
        if(!left && !zero)repeat(&o,' ',pad);
        for(size_t i=0;i<pn;i++)emit(&o,prefix[i]);
        if(!left && zero)repeat(&o,'0',pad);
        repeat(&o,'0',zeros);
        for(size_t i=0;i<n;i++)emit(&o,s[i]);
        if(left)repeat(&o,' ',pad);
        (void)numeric;
    }
    va_end(ap);
    if(cap)buf[o.count<cap?o.count:cap-1]=0;
    if(o.error){errno=EINVAL;return -1;}
    return (int)o.count;
}
int snprintf(char *b,size_t n,const char *f,...){va_list a;va_start(a,f);int r=vsnprintf(b,n,f,a);va_end(a);return r;}
int vsprintf(char *b,const char *f,va_list a){return vsnprintf(b,SIZE_MAX,f,a);}
int sprintf(char *b,const char *f,...){va_list a;va_start(a,f);int r=vsprintf(b,f,a);va_end(a);return r;}
int vfprintf(FILE *stream,const char *fmt,va_list ap) {
    char local[512];va_list cp;va_copy(cp,ap);
    int n=vsnprintf(local,sizeof(local),fmt,cp);va_end(cp);
    if(n<0)return -1;
    char *s=local;
    if((size_t)n>=sizeof(local)) {
        s=malloc((size_t)n+1);if(!s)return -1;
        va_copy(cp,ap);int again=vsnprintf(s,(size_t)n+1,fmt,cp);va_end(cp);
        if(again!=n){free(s);return -1;}
    }
    size_t written=fwrite(s,1,(size_t)n,stream);
    if(s!=local)free(s);
    return written==(size_t)n?n:-1;
}
int fprintf(FILE *s,const char *f,...){va_list a;va_start(a,f);int r=vfprintf(s,f,a);va_end(a);return r;}
int vprintf(const char *f,va_list a){return vfprintf(stdout,f,a);}
int printf(const char *f,...){va_list a;va_start(a,f);int r=vprintf(f,a);va_end(a);return r;}
