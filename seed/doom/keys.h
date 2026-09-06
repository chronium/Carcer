#ifndef DG_CX_KEYS_H
#define DG_CX_KEYS_H
#include "cx.h"
#include "doomkeys.h"
#include <string.h>
struct dg_keys { unsigned char raw[512],held[256];unsigned release; };
static unsigned char dg_map(unsigned code) {
    static const unsigned char plain[128]={
        [1]=KEY_ESCAPE,[2]='1',[3]='2',[4]='3',[5]='4',[6]='5',[7]='6',
        [8]='7',[9]='8',[10]='9',[11]='0',[12]='-',[13]='=',[14]=KEY_BACKSPACE,
        [15]=KEY_TAB,[16]='q',[17]='w',[18]='e',[19]='r',[20]='t',[21]='y',
        [22]='u',[23]='i',[24]='o',[25]='p',[26]='[',[27]=']',[28]=KEY_ENTER,
        [29]=KEY_RCTRL,[30]='a',[31]='s',[32]='d',[33]='f',[34]='g',[35]='h',
        [36]='j',[37]='k',[38]='l',[39]=';',[40]=39,[41]=96,[42]=KEY_RSHIFT,
        [43]=92,[44]='z',[45]='x',[46]='c',[47]='v',[48]='b',[49]='n',[50]='m',
        [51]=',',[52]='.',[53]='/',[54]=KEY_RSHIFT,[55]='*',[56]=KEY_RALT,[57]=' ',
        [58]=KEY_CAPSLOCK,[69]=KEY_NUMLOCK,[70]=KEY_SCRLCK,
        [71]=KEY_HOME,[72]=KEY_UPARROW,[73]=KEY_PGUP,[74]='-',[75]=KEY_LEFTARROW,
        [76]='5',[77]=KEY_RIGHTARROW,[78]='+',[79]=KEY_END,[80]=KEY_DOWNARROW,
        [81]=KEY_PGDN,[82]=KEY_INS,[83]=KEY_DEL
    };
    if((code>=59&&code<=68)||code==87||code==88)return (unsigned char)(128+code);
    if(code<128)return plain[code];
    switch(code){
    case 0x11c:return KEY_ENTER;
    case 0x11d:return KEY_RCTRL;
    case 0x135:return '/';
    case 0x137:return KEY_PRTSCR;
    case 0x138:return KEY_RALT;
    case 0x145:return KEY_PAUSE;
    case 0x147:return KEY_HOME;
    case 0x148:return KEY_UPARROW;
    case 0x149:return KEY_PGUP;
    case 0x14b:return KEY_LEFTARROW;
    case 0x14d:return KEY_RIGHTARROW;
    case 0x14f:return KEY_END;
    case 0x150:return KEY_DOWNARROW;
    case 0x151:return KEY_PGDN;
    case 0x152:return KEY_INS;
    case 0x153:return KEY_DEL;
    default:return 0;
    }
}
static void dg_lost(struct dg_keys *s){memset(s->raw,0,sizeof(s->raw));s->release=1;}
static int dg_release(struct dg_keys *s,int *pressed,unsigned char *key) {
    while(s->release && s->release<=256) {
        unsigned i=s->release++-1;
        if(s->held[i]){s->held[i]=0;*key=(unsigned char)i;*pressed=0;return 1;}
    }
    s->release=0;return 0;
}
static int dg_apply(struct dg_keys *s,const struct cx_key_event *e,int *pressed,unsigned char *key) {
    if(e->code>=512)return 0;
    unsigned char k=dg_map(e->code);
    if(!k || !!e->pressed==!!s->raw[e->code])return 0;
    s->raw[e->code]=!!e->pressed;
    if(e->pressed){if(s->held[k]++)return 0;}
    else {if(!s->held[k] || --s->held[k])return 0;}
    *key=k;*pressed=!!e->pressed;return 1;
}
#endif
