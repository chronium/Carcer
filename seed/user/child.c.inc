#include "cx.h"
static volatile unsigned char zeros[8193];
static volatile double a=1.25,b=2.5;
int main(void) {
    for(size_t i=0;i<sizeof(zeros);i++) if(zeros[i]) return 1;
    if(a*b!=3.125) return 2;
    char s[24]="abcdef";
    memmove(s+1,s,6);
    if(memcmp(s,"aabcdef",7)) return 3;
    memmove(s,s+1,6);
    if(memcmp(s,"abcdef",6)) return 4;
    memset(s+6,'!',3);
    if(strlen("sdk")!=3 || memcmp(s,"abcdef!!!",9)) return 5;
    return 37;
}
