#include <stdio.h>
#include <stdlib.h>
int main(int argc,char **argv) {
    if(argc!=3)return 1;
    unsigned long seconds=strtoul(argv[2],NULL,10);
    if(!seconds||seconds>3600)return 2;
    if(!cx_input_available())return 3;
    FILE *f=fopen(argv[1],"wb");if(!f)return 4;
    uint64_t cursor=0,start=cx_ticks();
    struct cx_key_event events[32];
    while(cx_ticks()-start<seconds*100) {
        uint64_t n=cx_keys(events,32,&cursor);
        if(n==CX_ERROR){fclose(f);return 5;}
        for(uint64_t i=0;i<n;i++)
            if(fprintf(f,"%lu %lu %03x %u %u\n",
                (unsigned long)events[i].sequence,(unsigned long)events[i].tick,
                events[i].code,events[i].pressed,events[i].flags)<0){fclose(f);return 6;}
        cx_sleep(1);
    }
    return fclose(f)?7:0;
}
