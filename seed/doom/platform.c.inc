#include "doomgeneric.h"
#include <stdio.h>
#include <string.h>
#include "cx.h"
#include "keys.h"
static struct cx_display display;
static unsigned frames,limit;
static const char *capture;
static uint64_t start_ticks,key_cursor;
void DG_Init(void) {
    if(cx_display_info(&display)!=sizeof(display)||display.width<DOOMGENERIC_RESX||display.height<DOOMGENERIC_RESY)exit(81);
    /* A newly launched client starts with fresh input, not launch keystrokes. */
    if(cx_input_available()==1){
        struct cx_key_event old[64];
        for(unsigned i=0;i<5;i++){
            uint64_t n=cx_keys(old,64,&key_cursor);
            if(n==CX_ERROR||!n)break;
        }
    }
    start_ticks=cx_ticks();
    printf("CX start_ticks=%lu input=%lu\n",(unsigned long)start_ticks,(unsigned long)(cx_input_available()==1));
}
void DG_DrawFrame(void) {
    if(cx_present(DG_ScreenBuffer,DOOMGENERIC_RESX*4,0,0,DOOMGENERIC_RESX,DOOMGENERIC_RESY)!=0)exit(82);
    ++frames;
    if(frames==1||!(frames%100))printf("CX progress frames=%u tick=%lu\n",frames,(unsigned long)cx_ticks());
    if(limit && frames==limit) {
        if(capture) {
            FILE *f=fopen(capture,"wb");
            size_t bytes=DOOMGENERIC_RESX*DOOMGENERIC_RESY*4;
            if(!f || fwrite(DG_ScreenBuffer,1,bytes,f)!=bytes || fclose(f)!=0)exit(83);
        }
        printf("CX frames=%u elapsed_ticks=%lu\n",frames,(unsigned long)(cx_ticks()-start_ticks));
        exit(0);
    }
}
void DG_SleepMs(uint32_t ms) {if(ms)cx_sleep(((uint64_t)ms+9)/10);}
uint32_t DG_GetTicksMs(void) {return (uint32_t)(cx_ticks()*10);}
int DG_GetKey(int *pressed,unsigned char *key) {
    static struct dg_keys state;
    static struct cx_key_event events[32];
    static unsigned at,count;
    for(;;) {
        if(dg_release(&state,pressed,key))return 1;
        if(at==count){
            if(cx_input_available()!=1)return 0;
            uint64_t n=cx_keys(events,32,&key_cursor);
            if(n==CX_ERROR||!n)return 0;
            at=0;count=(unsigned)n;
            if(events[0].flags&CX_KEY_LOST){dg_lost(&state);continue;}
        }
        if(dg_apply(&state,&events[at++],pressed,key))return 1;
    }
}
void DG_SetWindowTitle(const char *title) {printf("CX title: %s\n",title);}
int main(int argc,char **argv) {
    const char *log="runtime/doom.log";
    for(int i=1;i+1<argc;i++) {
        if(!strcmp(argv[i],"-cx-log"))log=argv[++i];
        else if(!strcmp(argv[i],"-cx-frames"))limit=(unsigned)strtoul(argv[++i],NULL,10);
        else if(!strcmp(argv[i],"-cx-capture"))capture=argv[++i];
    }
    if(cx_stdio(log,log)!=0)return 80;
    doomgeneric_Create(argc,argv);
    for(;;)doomgeneric_Tick();
}
