#include <stdio.h>
#include <stdlib.h>
#include <string.h>
int main(int argc,char **argv) {
    /* Generic supervisor: the first argument is a legacy executable; remaining
     * arguments are the second executable's complete argv vector. */
    if(argc<3||argc>32)return 1;
    struct cx_arg args[30];
    for(int i=2;i<argc;i++){args[i-2].data=argv[i];args[i-2].size=strlen(argv[i]);}
    uint64_t first=cx_spawn(argv[1],strlen(argv[1]));
    if(first==CX_ERROR)return 2;
    uint64_t second=cx_spawn_args(argv[2],strlen(argv[2]),args,(size_t)argc-2);
    uint64_t a,b;
    if(second==CX_ERROR){cx_wait(first,&a);return 3;}
    if(cx_wait(second,&b)!=1)return 4;
    if(cx_wait(first,&a)!=1)return 5;
    if(a)return 6;
    cx_exit(b);
}
