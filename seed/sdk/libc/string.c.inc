#include "cxlib.h"
int isspace(int c){return c==' ' || (c>='\t' && c<='\r');}
int isdigit(int c){return c>='0' && c<='9';}
int isupper(int c){return c>='A' && c<='Z';}
int islower(int c){return c>='a' && c<='z';}
int isalpha(int c){return isupper(c)||islower(c);}
int isalnum(int c){return isalpha(c)||isdigit(c);}
int isprint(int c){return c>=32 && c<=126;}
int isxdigit(int c){return isdigit(c)||(tolower(c)>='a'&&tolower(c)<='f');}
int toupper(int c){return islower(c)?c-32:c;}
int tolower(int c){return isupper(c)?c+32:c;}
char *strcpy(char *d,const char *s){char *r=d;while((*d++=*s++));return r;}
char *strncpy(char *d,const char *s,size_t n){size_t i=0;for(;i<n&&s[i];++i)d[i]=s[i];for(;i<n;++i)d[i]=0;return d;}
char *strcat(char *d,const char *s){strcpy(d+strlen(d),s);return d;}
char *strncat(char *d,const char *s,size_t n){char *p=d+strlen(d);while(n--&&*s)*p++=*s++;*p=0;return d;}
int strcmp(const char *a,const char *b){while(*a && *a==*b){++a;++b;}return (unsigned char)*a-(unsigned char)*b;}
int strncmp(const char *a,const char *b,size_t n){while(n){int d=(unsigned char)*a-(unsigned char)*b;if(d||!*a)return d;++a;++b;--n;}return 0;}
int strcasecmp(const char *a,const char *b){while(*a && tolower((unsigned char)*a)==tolower((unsigned char)*b)){++a;++b;}return tolower((unsigned char)*a)-tolower((unsigned char)*b);}
int strncasecmp(const char *a,const char *b,size_t n){while(n){int d=tolower((unsigned char)*a)-tolower((unsigned char)*b);if(d||!*a)return d;++a;++b;--n;}return 0;}
char *strchr(const char *s,int c){do{if((unsigned char)*s==(unsigned char)c)return (char *)s;}while(*s++);return NULL;}
char *strrchr(const char *s,int c){const char *r=NULL;do{if((unsigned char)*s==(unsigned char)c)r=s;}while(*s++);return (char *)r;}
char *strstr(const char *s,const char *n){size_t z=strlen(n);if(!z)return (char *)s;for(;*s;s++)if(!strncmp(s,n,z))return (char *)s;return NULL;}
char *strdup(const char *s){size_t n=strlen(s)+1;char *d=malloc(n);if(d)memcpy(d,s,n);return d;}
static int digit(int c){if(isdigit(c))return c-'0';c=tolower(c);return c>='a'&&c<='z'?c-'a'+10:99;}
static unsigned long number(const char *s,char **end,int base,int *negative,int *over) {
    const char *start=s;unsigned long v=0;
    *negative=*over=0;
    if(base && (base<2||base>36)){errno=EINVAL;if(end)*end=(char *)s;return 0;}
    while(isspace((unsigned char)*s))++s;
    if(*s=='-'||*s=='+')*negative=(*s++=='-');
    if((base==0||base==16)&&s[0]=='0'&&(s[1]=='x'||s[1]=='X')&&digit((unsigned char)s[2])<16){base=16;s+=2;}
    if(!base)base=*s=='0'?8:10;
    const char *first=s;
    while(digit((unsigned char)*s)<base) {
        unsigned d=(unsigned)digit((unsigned char)*s++);
        if(v>(ULONG_MAX-d)/(unsigned)base)*over=1;
        else v=v*(unsigned)base+d;
    }
    if(end)*end=(char *)(s==first?start:s);
    return v;
}
unsigned long strtoul(const char *s,char **e,int base){int neg,over;unsigned long v=number(s,e,base,&neg,&over);if(over){errno=ERANGE;return ULONG_MAX;}return neg?0ul-v:v;}
long strtol(const char *s,char **e,int base){int neg,over;unsigned long v=number(s,e,base,&neg,&over),limit=neg?(unsigned long)LONG_MAX+1:(unsigned long)LONG_MAX;if(over||v>limit){errno=ERANGE;return neg?LONG_MIN:LONG_MAX;}return neg?(long)(0ul-v):(long)v;}
/* Locale-independent decimal conversion, no hexadecimal/NaN/Inf grammar. */
double strtod(const char *s,char **end) {

    const char *start=s;
    while(isspace((unsigned char)*s))++s;
    int neg=0,any=0,significant=0,exponent=0;
    if(*s=='-'||*s=='+')neg=(*s++=='-');
    double v=0;
    while(isdigit((unsigned char)*s)){
        unsigned d=(unsigned)(*s++-'0');any=1;
        if(significant<19 && (d||significant)){v=v*10+d;++significant;}
        else if(significant && exponent<100000)++exponent;
    }
    if(*s=='.'){
        ++s;
        while(isdigit((unsigned char)*s)){
            unsigned d=(unsigned)(*s++-'0');any=1;
            if(significant<19){
                if(exponent>-100000)--exponent;
                if(d||significant){v=v*10+d;++significant;}
            }
        }
    }
    if(!any){if(end)*end=(char *)start;return 0;}
    if(*s=='e'||*s=='E'){
        const char *e=s++;int eneg=0,n=0;
        if(*s=='-'||*s=='+')eneg=(*s++=='-');
        if(!isdigit((unsigned char)*s))s=e;
        else {
            while(isdigit((unsigned char)*s)){
                if(n<100000)n=n*10+(*s-'0');
                ++s;
            }
            exponent+=eneg?-n:n;
        }
    }
    if(end)*end=(char *)s;
    if(v){
        if(exponent>400){v=__builtin_inf();errno=ERANGE;}
        else if(exponent < -400){v=0;errno=ERANGE;}
        else {
            while(exponent>=100){v*=1e100;exponent-=100;}
            while(exponent<=-100){v/=1e100;exponent+=100;}
            while(exponent>0){v*=10;--exponent;}
            while(exponent<0){v/=10;++exponent;}
            if(v==0||v==__builtin_inf())errno=ERANGE;
        }
    }
    return neg?-v:v;

}
int atoi(const char *s){return (int)strtol(s,NULL,10);}
long atol(const char *s){return strtol(s,NULL,10);}
double atof(const char *s){return strtod(s,NULL);}
