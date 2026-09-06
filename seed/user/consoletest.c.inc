#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#define C(x) do{if(!(x))cx_exit(__LINE__);}while(0)
static const char *script_path="test/con-script",*log_path="test/con-log";
static void put(const char *p,const char *s){
    size_t n=strlen(p);C(cx_create(p,n)==0);
    C(cx_write(p,n,0,s,strlen(s))==strlen(s));
}
static void remove_file(const char *p){C(cx_remove(p,strlen(p))==0);}
static uint64_t run(const char *script,const char *capture) {
    put(script_path,script);
    struct cx_arg args[]={
        {"console",7},{"--script",8},{script_path,15},{log_path,12},{capture,0}
    };
    if(capture)args[4].size=strlen(capture);
    uint64_t child=cx_spawn_args("seed/user/console.cxe",21,args,capture?5:4),status=0;
    C(child!=CX_ERROR&&cx_wait(child,&status)==1);
    return status;
}
static char *log_read(void){
    uint64_t n=cx_size(log_path,strlen(log_path));C(n!=CX_ERROR&&n<65536);
    char *s=malloc((size_t)n+1);C(s);
    C(cx_read(log_path,strlen(log_path),0,s,(size_t)n)==n);s[n]=0;return s;
}
static void cleanup(void){remove_file(script_path);remove_file(log_path);}
int main(void) {
    char *log;
    C(run("write test/immutable nope\nexit 0\n",0)==125);
    log=log_read();C(strstr(log,"error: command failed\n"));free(log);cleanup();
    C(run("write test/con-bad 'unclosed\n",0)==125);cleanup();
    C(cx_size("test/con-bad",12)==CX_ERROR);
    C(run("write test/con-bad should-not-execute",0)==125);cleanup();
    C(cx_size("test/con-bad",12)==CX_ERROR);
    C(run("run missing-program\n",0)==125);cleanup();
    C(run("exit 18446744073709551616\n",0)==125);cleanup();
    C(run("exit 18446744073709551615\n",0)==UINT64_MAX);cleanup();
    /* Output failure during a mutating command's echo must prevent dispatch.
     * The real1MiB ceiling is reached using escaped4096-byte file reads. */
    char *fill=malloc(4097),*commands=malloc(4096);C(fill&&commands);
    memset(fill,1,4096);fill[4096]=0;put("test/con-fill",fill);
    put("test/con-target","keep");
    const char *cat="cat test/con-fill\n",*padcat="cat test/con-pad\n";
    const char *banner="CodexOS console\nType help for commands.\n";
    size_t used=strlen(banner)+63*(2+strlen(cat)+4096*4+1);
    size_t encoded=1048576-used-(2+strlen(padcat))-1-5;
    size_t padlen=encoded/4+encoded%4;
    C(padlen<=4096);
    memset(fill,1,encoded/4);
    memset(fill+encoded/4,'p',encoded%4);fill[padlen]=0;
    put("test/con-pad",fill);
    commands[0]=0;
    for(unsigned i=0;i<63;i++)strcat(commands,cat);
    strcat(commands,padcat);strcat(commands,"rm test/con-target\n");
    C(run(commands,0)==125);
    C(cx_size(log_path,strlen(log_path))==1048576);
    C(cx_size("test/con-target",15)==4);
    char kept[4];C(cx_read("test/con-target",15,0,kept,4)==4&&!memcmp(kept,"keep",4));
    cleanup();remove_file("test/con-fill");remove_file("test/con-pad");
    free(fill);free(commands);
    /* Unlink preserves the open transcript. Subsequent commands still run;
     * the detached transcript is reclaimed when the console closes/exits. */
    C(run("rm test/con-log\nrm test/con-target\n",0)==0);
    C(cx_size(log_path,strlen(log_path))==CX_ERROR);
    C(cx_size("test/con-target",15)==CX_ERROR);
    remove_file(script_path);
    C(run("runfor 0 seed/user/spin.cxe\n",0)==125);cleanup();
    C(run("runfor 18446744073709551615 seed/user/spin.cxe\n",0)==125);cleanup();
    C(run("runfor nope seed/user/spin.cxe\n",0)==125);cleanup();
    C(run("runfor 10 missing-program\n",0)==125);cleanup();
    for(unsigned i=0;i<8;i++){
        C(run("runfor 2 seed/user/spin.cxe\nrunfor 100 seed/user/child.cxe\nexit 0\n",0)==0);
        log=log_read();C(strstr(log,"timeout\n")&&strstr(log,"status=37\n"));
        free(log);cleanup();
    }
    C(run("runfor 100 seed/user/concurrent.cxe seed/user/spin.cxe missing-program\nexit 0\n",0)==0);
    log=log_read();C(strstr(log,"status=3\n")&&!strstr(log,"timeout\n"));free(log);cleanup();
    C(run("runfor 3 seed/user/concurrent.cxe seed/user/spin.cxe seed/user/spin.cxe\nrun seed/user/child.cxe\nexit 0\n",0)==0);
    log=log_read();C(strstr(log,"timeout\n")&&strstr(log,"status=37\n"));free(log);cleanup();
    const char *script=
        "write 'test/con file' 'hello world'\n"
        "stat 'test/con file'\n"
        "cat 'test/con file'\n"
        "mv 'test/con file' test/con-moved\n"
        "ls test/con-moved\n"
        "run seed/user/child.cxe\n"
        "run seed/user/argchild.cxe s\n"
        "run seed/user/argchild.cxe f\n"
        "rm test/con-moved\n"
        "ticks\n"
        "exit 0\n";
    C(run(script,"test/console-frame")==0);
    log=log_read();
    C(strstr(log,"size=11 attributes=0\n")&&strstr(log,"hello world\n")&&
      strstr(log,"rw 11 \"test/con-moved\"\n")&&strstr(log,"status=37\n")&&
      strstr(log,"status=1311768467139281697\n")&&strstr(log,"status=18446744073709551615\n"));
    free(log);cleanup();
    C(cx_size("test/con file",13)==CX_ERROR&&cx_size("test/con-moved",14)==CX_ERROR);
    /* Captured renderer is also compared to the real framebuffer by boot observer. */
    struct cx_display d;C(cx_display_info(&d)==sizeof(d));
    uint32_t w=(d.width<800?d.width:800)/6*6,h=(d.height<480?d.height:480)/8*8;
    C(cx_size("test/console-frame",18)==(uint64_t)w*h*4);
    return 0;
}
