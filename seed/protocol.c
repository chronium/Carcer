#include "protocol.h"
#include "serial.h"
#include "tools.h"
typedef unsigned char u8;typedef unsigned short u16;typedef unsigned u32;typedef unsigned long u64;
#define FH 16u
#define PV 1u
#define FO 25u
#define QO 27u
#define RB \
(16u*1024u+256u+QO)
static u16 r16(const u8*b){return(u16)b[0]|((u16)b[1]<<8);}static u32 r32(const u8*b){return(u32)b[0]|((u32)b[1]<<8)|((u32)b[2]<<16)|((u32)b[3]<<24);}static void w16(u8*b,u16 v){b[0]=(u8)v;b[1]=(u8)(v>>8);}static void w32(u8*b,u32 v){b[0]=(u8)v;b[1]=(u8)(v>>8);b[2]=(u8)(v>>16);b[3]=(u8)(v>>24);}static void db(u32 n){for(u32 i=0;i<n;++i){(void)srd();}}void ph(u16 mt,u32 ri,u32 pn){u8 h[FH];h[0]='C';h[1]='X';h[2]='O';h[3]='S';w16(h+4,PV);w16(h+6,mt);w32(h+8,ri);w32(h+12,pn);swb(h,sizeof(h));}void pw16(u16 v){u8 b[2];w16(b,v);swb(b,sizeof(b));}void pw32(u32 v){u8 b[4];w32(b,v);swb(b,sizeof(b));}__attribute__((noreturn))void ploop(void){u8 h[FH];u8 p[RB];for(;;){for(u32 i=0;i<sizeof(h);++i){h[i]=srd();}u16 ver=r16(h+4);u16 mt=r16(h+6);u32 ri=r32(h+8);u32 pn=r32(h+12);if(pn>FM){for(;;){__asm__ volatile("pause");}}int vh=h[0]=='C'&&h[1]=='X'&&h[2]=='O'&&h[3]=='S'&&ver==PV;if(pn>sizeof(p)){db(pn);if(vh&&mt==INVOKE_TOOL_REQUEST&&ri!=0){tlsfail(ri);}continue;}for(u32 i=0;i<pn;++i){p[i]=srd();}if(!vh||ri==0){continue;}if(mt==LIST_TOOLS_REQUEST&&pn==0){tlslist(ri);}else if(mt==INVOKE_TOOL_REQUEST){tlshandle(ri,p,pn);}}}
