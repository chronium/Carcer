#include "cxlib.h"
/* scanf owns lexical recognition: conversion functions intentionally roll back
 * incomplete prefixes/exponents, whereas a scanning input item must fail.
 * Width bounds the item before any destination is assigned. Zero means a
 * missing/incomplete item; the caller stops scanning without assignments. */
static size_t numeric_item(const char *s,size_t limit,char spec) {
    size_t i=0;
    if(i<limit && (s[i]=='+'||s[i]=='-'))++i;
    if(strchr("fFeEgG",spec)) {
        size_t digits=0;
        while(i<limit && isdigit((unsigned char)s[i])){++i;++digits;}
        if(i<limit && s[i]=='.'){
            ++i;while(i<limit && isdigit((unsigned char)s[i])){++i;++digits;}
        }
        if(!digits)return 0;
        if(i<limit && (s[i]=='e'||s[i]=='E')){
            ++i;
            if(i<limit && (s[i]=='+'||s[i]=='-'))++i;
            size_t first=i;
            while(i<limit && isdigit((unsigned char)s[i]))++i;
            if(i==first)return 0;
        }
        return i;
    }
    int base=spec=='o'?8:(spec=='x'||spec=='X')?16:10;
    if((spec=='i'||base==16) && i+1<limit && s[i]=='0' &&
       (s[i+1]=='x'||s[i+1]=='X')){
        i+=2;size_t first=i;
        while(i<limit && isxdigit((unsigned char)s[i]))++i;
        return i==first?0:i;
    }
    if(spec=='i')base=i<limit&&s[i]=='0'?8:10;
    size_t first=i;
    while(i<limit){
        unsigned char c=(unsigned char)s[i];
        int digit=isdigit(c)?c-'0':tolower(c)>='a'&&tolower(c)<='f'?tolower(c)-'a'+10:99;
        if(digit>=base)break;
        ++i;
    }
    return i==first?0:i;
}
int vsscanf(const char *input,const char *fmt,va_list args) {
    const char *s=input;int assigned=0,conversions=0,input_failure=0;
    va_list ap;va_copy(ap,args);
    while(*fmt) {
        if(isspace((unsigned char)*fmt)){while(isspace((unsigned char)*fmt))++fmt;while(isspace((unsigned char)*s))++s;continue;}
        if(*fmt!='%'){if(*s!=*fmt){input_failure=!*s;break;}++s;++fmt;continue;}
        ++fmt;
        if(*fmt=='%'){if(*s!='%'){input_failure=!*s;break;}++s;++fmt;continue;}
        int suppress=0,width=0,length=0;
        if(*fmt=='*'){suppress=1;++fmt;}
        while(isdigit((unsigned char)*fmt)){if(width>(INT_MAX-9)/10)goto done;width=width*10+(*fmt++-'0');}
        if(*fmt=='h'){length=-1;++fmt;if(*fmt=='h'){length=-2;++fmt;}}
        else if(*fmt=='l'){length=1;++fmt;if(*fmt=='l'){length=2;++fmt;}}
        else if(*fmt=='z'||*fmt=='t'||*fmt=='j'){length=2;++fmt;}
        char spec=*fmt;if(!spec)break;++fmt;
        if(spec=='n') {
            if(!suppress){
                size_t n=(size_t)(s-input);
                if(length>0)*va_arg(ap,long *)=(long)n;
                else if(length==-1)*va_arg(ap,short *)=(short)n;
                else if(length==-2)*va_arg(ap,signed char *)=(signed char)n;
                else *va_arg(ap,int *)=(int)n;
            }
            ++conversions;continue;
        }
        if(spec!='c'&&spec!='[')while(isspace((unsigned char)*s))++s;
        if(!*s){input_failure=1;break;}
        if(spec=='s'||spec=='c'||spec=='[') {
            unsigned char set[256];int inverse=0;
            if(spec=='[') {
                memset(set,0,sizeof(set));
                if(*fmt=='^'){inverse=1;++fmt;}
                if(*fmt==']'){set[']']=1;++fmt;}
                while(*fmt && *fmt!=']'){
                    unsigned char a=(unsigned char)*fmt++;
                    if(*fmt=='-'&&fmt[1]&&fmt[1]!=']'){
                        ++fmt;unsigned char b=(unsigned char)*fmt++;
                        if(a>b)goto done;
                        for(unsigned i=a;i<=b;i++)set[i]=1;
                    }else set[a]=1;
                }
                if(*fmt!=']')goto done;
                ++fmt;
            }
            size_t n=0,limit=width?(size_t)width:spec=='c'?1:SIZE_MAX;
            while(n<limit&&s[n]) {
                unsigned char c=(unsigned char)s[n];
                if(spec=='s'&&isspace(c))break;
                if(spec=='['&&(!set[c])!=inverse)break;
                ++n;
            }
            if(!n)break;
            if(spec=='c'&&n<limit){input_failure=1;break;}
            if(!suppress){char *d=va_arg(ap,char *);memcpy(d,s,n);if(spec!='c')d[n]=0;++assigned;}
            s+=n;++conversions;continue;
        }
        if(!strchr("diuoxXfFeEgG",spec))break;
        size_t item=numeric_item(s,width?(size_t)width:SIZE_MAX,spec);
        if(!item)break;
        char *copy=malloc(item+1);if(!copy)break;
        memcpy(copy,s,item);copy[item]=0;
        const char *number=copy;char *end;
        if(strchr("fFeEgG",spec)){
            double value=strtod(number,&end);
            if(end!=number+item){free(copy);break;}
            if(!suppress){if(length==1)*va_arg(ap,double *)=value;else if(!length)*va_arg(ap,float *)=(float)value;else {free(copy);break;}++assigned;}
        }else{
            int base=spec=='i'?0:spec=='o'?8:(spec=='x'||spec=='X')?16:10;
            int signed_value=spec=='i'||spec=='d';
            unsigned long value=signed_value?(unsigned long)strtol(number,&end,base):strtoul(number,&end,base);
            if(end!=number+item){free(copy);break;}
            if(!suppress){
                if(signed_value){
                    if(length==2)*va_arg(ap,long long *)=(long long)value;
                    else if(length==1)*va_arg(ap,long *)=(long)value;
                    else if(length==-1)*va_arg(ap,short *)=(short)value;
                    else if(length==-2)*va_arg(ap,signed char *)=(signed char)value;
                    else *va_arg(ap,int *)=(int)value;
                }else{
                    if(length==2)*va_arg(ap,unsigned long long *)=(unsigned long long)value;
                    else if(length==1)*va_arg(ap,unsigned long *)=value;
                    else if(length==-1)*va_arg(ap,unsigned short *)=(unsigned short)value;
                    else if(length==-2)*va_arg(ap,unsigned char *)=(unsigned char)value;
                    else *va_arg(ap,unsigned *)=(unsigned)value;
                }
                ++assigned;
            }
        }
        s+=item;free(copy);++conversions;
    }
done:
    va_end(ap);return !conversions&&input_failure?EOF:assigned;
}
int sscanf(const char *s,const char *f,...){va_list a;va_start(a,f);int r=vsscanf(s,f,a);va_end(a);return r;}
