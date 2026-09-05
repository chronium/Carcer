#include "cx.h"
/* A launch file contains 1..32 newline-terminated lines. First line is both
 * executable path and argv[0]; subsequent lines are literal arguments.
 * No quoting, shell expansion, environment or comment syntax. */
int main(int argc,char **argv) {
    const char *config="runtime/launch.txt";
    if(argc==2) config=argv[1];
    else if(argc!=0) return 120;
    size_t cn=strlen(config);
    uint64_t size=cx_size(config,cn);
    if(!size || size>4096) return 121;
    char data[4096];
    if(cx_read(config,cn,0,data,(size_t)size)!=size) return 121;
    struct cx_arg args[32];
    size_t count=0,start=0;
    for(size_t i=0;i<size;++i) {
        if(!data[i]) return 122;
        if(data[i]!='\n') continue;
        if(count==32) return 122;
        args[count].data=data+start;
        args[count++].size=i-start;
        start=i+1;
    }
    if(start!=size || !count || !args[0].size || args[0].size>255)
        return 122;
    uint64_t task=cx_spawn_args(args[0].data,args[0].size,args,count);
    if(task==CX_ERROR) return 123;
    uint64_t status;
    if(cx_wait(task,&status)!=1) return 124;
    cx_exit(status);
}
