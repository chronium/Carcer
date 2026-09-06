#pragma GCC target("general-regs-only")
#include "input.h"
#include "interrupts.h"
typedef unsigned char u8;
typedef unsigned short u16;
typedef unsigned u32;
typedef unsigned long u64;
#define HISTORY 256u
static struct key_event history[HISTORY];
static u8 down[512],extended,pause_at,available;
static u64 next=1;
static u8 fixture_active;
static const u8 pause_tail[]={0x1d,0x45,0xe1,0x9d,0xc5};
_Static_assert(sizeof(struct key_event)==24,"key event layout");
static u8 in(u16 p){u8 v;__asm__ volatile("inb %1,%0":"=a"(v):"Nd"(p));return v;}
static void out(u16 p,u8 v){__asm__ volatile("outb %0,%1"::"a"(v),"Nd"(p));}
static void reset(void) {
    next=1;extended=pause_at=0;
    for(u32 i=0;i<512;i++)((volatile u8 *)down)[i]=0;
}
static void publish(u16 code,u8 pressed,u64 tick) {
    if(!next||!code||code>=512)return;
    struct key_event *e=&history[(next-1)%HISTORY];
    e->sequence=next++;e->tick=tick;e->code=code;e->pressed=pressed;
    e->flags=pressed&&down[code]?1:0;e->reserved=0;
    down[code]=pressed;
    /* Exhaustion after 2^64 events stops publication rather than aliasing. */
    if(!next)available=0;
}
static void decode(u8 c,u64 tick) {
    if(pause_at) {
        if(c==pause_tail[pause_at-1]) {
            if(++pause_at==6){pause_at=0;publish(0x145,1,tick);publish(0x145,0,tick);}
            return;
        }
        pause_at=extended=0;
    }
    if(c==0xe1){pause_at=1;extended=0;return;}
    if(c==0xe0){extended=1;return;}
    if(c==0xfa||c==0xfe||c==0x00||c==0xff){extended=0;return;}
    u16 code=(u16)(c&0x7f)|(extended?0x100:0);
    extended=0;
    /* PrintScreen fake shifts do not represent physical shift transitions. */
    if(code==0x12a||code==0x136)return;
    publish(code,(c&0x80)?0:1,tick);
}
int key_available(void){return available;}
int key_read(u64 cursor,struct key_event *out_events,u32 capacity,u64 *after) {
    if(!capacity||capacity>KEY_BATCH||cursor>next)return -1;
    u64 oldest=next>HISTORY?next-HISTORY:1;
    int lost=cursor && cursor<oldest;
    if(!cursor||cursor<oldest)cursor=oldest;
    u32 n=0;
    while(cursor<next && n<capacity) {
        const struct key_event *e=&history[(cursor-1)%HISTORY];
        out_events[n].sequence=e->sequence;out_events[n].tick=e->tick;
        out_events[n].code=e->code;out_events[n].pressed=e->pressed;
        out_events[n].flags=e->flags|((!n&&lost)?2:0);
        out_events[n].reserved=0;
        ++cursor;++n;
    }
    *after=cursor;return (int)n;
}
void key_poll(void) {
    if(!available||fixture_active)return;
    for(u32 n=0;n<16;n++){
        u8 status=in(0x64);
        if(!(status&1))break;
        u8 c=in(0x60);
        if(status&0xe0){if(!(status&0x20))extended=pause_at=0;continue;}
        decode(c,tnow());
    }
}
static int writable(void){for(u32 n=0;n<100000;n++){u8 s=in(0x64);if(s==0xff)return 0;if(!(s&2))return 1;}return 0;}
static int command(u8 c){if(!writable())return 0;out(0x64,c);return 1;}
static int data(u8 c){if(!writable())return 0;out(0x60,c);return 1;}
static int response(u8 *c){for(u32 n=0;n<100000;n++){u8 s=in(0x64);if(s==0xff)return 0;if(s&1){u8 b=in(0x60);if(s&0xe0)continue;*c=b;return 1;}}return 0;}
static int device(u8 c){for(u32 n=0;n<3;n++){u8 reply;if(!data(c)||!response(&reply))return 0;if(reply==0xfa)return 1;if(reply!=0xfe)return 0;}return 0;}
int key_init(void) {
    available=0;reset();
    if(!command(0xad))return 0;
    for(u32 i=0;i<32&&(in(0x64)&1);i++)(void)in(0x60);
    u8 config;
    if(!command(0x20)||!response(&config))return 0;
    /* Set-2 keyboard bytes translated to set 1, serviced by bounded PIT polls.
     * Keyboard IRQ stays masked; no second interrupt frame is introduced. */
    config=(u8)((config|0x40)&~0x11);
    if(!command(0x60)||!data(config)||!command(0xae)||
       !device(0xf5)||!device(0xf0)||!device(2)||!device(0xf4))return 0;
    available=1;return 1;
}
int key_tests(void) {
    reset();
    struct key_event e[KEY_BATCH];u64 cursor=0;
    decode(0x1e,3);decode(0x1e,4);decode(0x9e,5);
    decode(0xe0,6);decode(0x48,6);decode(0xe0,7);decode(0xc8,7);
    decode(0xe1,8);for(u32 i=0;i<5;i++)decode(pause_tail[i],8);
    decode(0xe0,9);decode(0x2a,9);decode(0xe0,9);decode(0x37,9);
    decode(0xe0,10);decode(0xb7,10);decode(0xe0,10);decode(0xaa,10);
    if(key_read(0,e,64,&cursor)!=9||cursor!=10)return 0;
    if(e[0].code!=0x1e||!e[0].pressed||e[0].flags||e[1].flags!=1||
       e[2].pressed||e[3].code!=0x148||e[4].pressed||
       e[5].code!=0x145||!e[5].pressed||e[6].pressed||
       e[7].code!=0x137||e[8].pressed)return 0;
    u64 other=0;
    if(key_read(0,e,2,&other)!=2||other!=3||e[0].sequence!=1||
       key_read(cursor,e,64,&other)!=0||other!=cursor)return 0;
    decode(0xe1,11);decode(0x1e,12);
    for(u32 i=0;i<300;i++)decode((i&1)?0x9e:0x1e,20+i);
    u64 oldest=next-HISTORY;
    if(key_read(1,e,64,&cursor)!=64||e[0].sequence!=oldest||
       !(e[0].flags&2)||e[1].flags&2||cursor!=oldest+64)return 0;
    if(key_read(next+1,e,1,&cursor)!=-1||key_read(0,e,0,&cursor)!=-1)return 0;
    reset();return 1;
}

/* Boot-only deterministic backend for syscall boundary regression.
 * Caller holds CLI at begin/end; normal operation never enables this state.
 * Hardware polling is paused while the fixture executes as ordinary userland. */
static u8 saved_available,saved_extended,saved_pause,saved_down[512];
static u64 saved_next;
static struct key_event saved_history[HISTORY];
int key_fixture(int enable) {
    if(enable) {
        if(fixture_active)return 0;
        saved_available=available;saved_extended=extended;saved_pause=pause_at;
        saved_next=next;
        for(u32 i=0;i<512;i++)saved_down[i]=down[i];
        for(u32 i=0;i<HISTORY;i++){
            saved_history[i].sequence=history[i].sequence;saved_history[i].tick=history[i].tick;
            saved_history[i].code=history[i].code;saved_history[i].pressed=history[i].pressed;
            saved_history[i].flags=history[i].flags;saved_history[i].reserved=history[i].reserved;
        }
        fixture_active=1;reset();available=1;
        decode(0x1e,123);decode(0x9e,124);
    }else{
        if(!fixture_active)return 0;
        for(u32 i=0;i<512;i++)down[i]=saved_down[i];
        for(u32 i=0;i<HISTORY;i++){
            history[i].sequence=saved_history[i].sequence;history[i].tick=saved_history[i].tick;
            history[i].code=saved_history[i].code;history[i].pressed=saved_history[i].pressed;
            history[i].flags=saved_history[i].flags;history[i].reserved=saved_history[i].reserved;
        }
        next=saved_next;available=saved_available;extended=saved_extended;pause_at=saved_pause;
        fixture_active=0;
    }
    return 1;
}
