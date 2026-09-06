#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "../console/core.h"
#include "../console/font.h"
static struct con_screen screen;
static struct con_keyboard keyboard;
static uint32_t *pixels,width,height;
static FILE *transcript;
static char pending[512];
static size_t pending_n,total_out;
static int failed,interactive,quit;
static uint64_t exit_status,key_cursor;
static char line[CON_LINE];
static size_t line_n;
static int line_overflow;
static int flush(void) {
    if(pending_n&&fwrite(pending,1,pending_n,transcript)!=pending_n)failed=1;
    pending_n=0;return !failed;
}
static void output(const char *text) {
    while(*text&&!failed) {
        unsigned char c=(unsigned char)*text++;
        if(total_out++==1048576){failed=1;break;}
        con_put(&screen,c);
        pending[pending_n++]=(char)c;
        if(pending_n==sizeof(pending))flush();
    }
}
static void number(uint64_t n) {
    char text[32];snprintf(text,sizeof(text),"%lu",(unsigned long)n);output(text);
}
static void escaped(const unsigned char *p,size_t n,int name) {
    static const char hex[]="0123456789abcdef";
    if(name)output("\"");
    for(size_t i=0;i<n;i++) {
        unsigned c=p[i];char b[5]={0};
        if(c=='\n'&&!name){output("\n");continue;}
        if(c=='\\'||c=='"'){b[0]='\\';b[1]=(char)c;}
        else if(c>=32&&c<127)b[0]=(char)c;
        else {b[0]='\\';b[1]='x';b[2]=hex[c>>4];b[3]=hex[c&15];}
        output(b);
    }
    if(name)output("\"");
}
static void glyph(unsigned x,unsigned y,unsigned char c,uint32_t color) {
    if(c<32||c>126)c='?';
    for(unsigned row=0;row<7;row++)for(unsigned col=0;col<5;col++)
        if(con_font[c-32][row]&(16u>>col))pixels[(y+row)*width+x+col]=color;
}
static int paint(void) {
    if(!flush())return 0;
    for(size_t i=0;i<(size_t)width*height;i++)pixels[i]=0x101820;
    for(unsigned y=0;y<screen.rows;y++)for(unsigned x=0;x<screen.cols;x++)
        glyph(x*6,y*8,(unsigned char)screen.cells[y*screen.cols+x],0xe1e8eb);
    if(interactive&&!quit) {
        unsigned y=screen.rows*8;glyph(0,y,'>',0x60d9aa);
        size_t room=screen.cols-3,start=line_n>room?line_n-room:0;
        for(size_t i=start;i<line_n;i++)glyph((unsigned)(i-start+2)*6,y,(unsigned char)line[i],0xffffff);
        glyph((unsigned)(line_n-start+2)*6,y,'_',0x60d9aa);
    }
    if(cx_present(pixels,width*4,0,0,width,height)!=0){failed=1;return 0;}
    return 1;
}
static int init_screen(void) {
    struct cx_display d;
    if(cx_display_info(&d)!=sizeof(d)||d.width<120||d.height<48)return 0;
    width=(d.width<800?d.width:800)/6*6;
    height=(d.height<480?d.height:480)/8*8;
    screen.cols=width/6;screen.rows=height/8-1;
    screen.cells=malloc(screen.cols*screen.rows);
    pixels=malloc((size_t)width*height*4);
    if(!screen.cells||!pixels)return 0;
    con_clear(&screen);return 1;
}
static int path_ok(const char *p){size_t n=strlen(p);return n>0&&n<=255;}
static int command(int argc,char **argv) {
    if(!argc)return 1;
    const char *cmd=argv[0];
    if(!strcmp(cmd,"help")&&argc==1) {
        output("help | ls [prefix] | cat path | stat path\n"
               "write path text | rm path | mv old new\n"
               "run executable [args...] | ticks | clear\n"
               "exit [status]\n"
               "Quote spaces with single/double quotes.\n"
               "write creates a new file; cat shows <=4096 bytes.\n"
               "run waits; child output stays in its own files.\n");return 1;
    }
    if(!strcmp(cmd,"ticks")&&argc==1){number(cx_ticks());output("\n");return 1;}
    if(!strcmp(cmd,"clear")&&argc==1){con_clear(&screen);return 1;}
    if(!strcmp(cmd,"exit")&&(argc==1||argc==2)) {
        uint64_t value=0;
        if(argc==2){
            if(!argv[1][0])return 0;
            for(const char *p=argv[1];*p;p++){
                if(*p<'0'||*p>'9'||value>(UINT64_MAX-(unsigned)(*p-'0'))/10)return 0;
                value=value*10+(unsigned)(*p-'0');
            }
        }
        exit_status=value;quit=1;return 1;
    }
    if(!strcmp(cmd,"ls")&&(argc==1||argc==2)) {
        struct cx_file_record *list=malloc(128*sizeof(*list));
        if(!list)return 0;
        uint64_t n=cx_files(list,128);
        if(n==CX_ERROR){free(list);return 0;}
        size_t prefix=argc==2?strlen(argv[1]):0;
        for(uint64_t i=0;i<n;i++)if(prefix<=list[i].length&&
            (!prefix||!memcmp(list[i].path,argv[1],prefix))) {
            output(list[i].attributes&1?"ro ":"rw ");number(list[i].size);output(" ");
            escaped(list[i].path,list[i].length,1);output("\n");
        }
        free(list);return 1;
    }
    if(!strcmp(cmd,"stat")&&argc==2&&path_ok(argv[1])) {
        size_t n=strlen(argv[1]);uint64_t z=cx_size(argv[1],n),a=cx_attributes(argv[1],n);
        if(z==CX_ERROR||a==CX_ERROR)return 0;
        output("size=");number(z);output(" attributes=");number(a);output("\n");return 1;
    }
    if(!strcmp(cmd,"cat")&&argc==2&&path_ok(argv[1])) {
        unsigned char data[4096];size_t n=strlen(argv[1]);uint64_t z=cx_size(argv[1],n);
        if(z==CX_ERROR)return 0;
        size_t amount=z>sizeof(data)?sizeof(data):(size_t)z;
        if(cx_read(argv[1],n,0,data,amount)!=amount)return 0;
        escaped(data,amount,0);output("\n");
        if(z>amount)output("[first 4096 bytes]\n");
        return 1;
    }
    if(!strcmp(cmd,"write")&&argc==3&&path_ok(argv[1])) {
        size_t n=strlen(argv[1]),z=strlen(argv[2]);
        if(cx_create(argv[1],n)!=0)return 0;
        return cx_write(argv[1],n,0,argv[2],z)==z;
    }
    if(!strcmp(cmd,"rm")&&argc==2&&path_ok(argv[1]))
        return cx_remove(argv[1],strlen(argv[1]))==0;
    if(!strcmp(cmd,"mv")&&argc==3&&path_ok(argv[1])&&path_ok(argv[2]))
        return cx_rename(argv[1],strlen(argv[1]),argv[2],strlen(argv[2]))==0;
    if(!strcmp(cmd,"run")&&argc>=2&&path_ok(argv[1])) {
        struct cx_arg args[CON_ARGS];
        for(int i=1;i<argc;i++){args[i-1].data=argv[i];args[i-1].size=strlen(argv[i]);}
        if(!paint())return 0;
        uint64_t child=cx_spawn_args(argv[1],strlen(argv[1]),args,(unsigned)(argc-1)),status;
        if(child==CX_ERROR)return 0;
        /* No drawing or keyboard reads while the foreground program runs. */
        if(cx_wait(child,&status)!=1)return 0;
        output("status=");number(status);output("\n");return 1;
    }
    return 0;
}
static int execute(void) {
    char *args[CON_ARGS];
    output("> ");output(line);output("\n");
    /* Record the complete echo before dispatching any side effects. */
    if(!flush())return 0;
    int argc=con_parse(line,args);
    int ok=argc>=0&&command(argc,args);
    if(!ok)output("error: command failed\n");
    line_n=0;line[0]=0;
    return ok&&!failed;
}
static void discard_keys(void) {
    struct cx_key_event events[64];
    /* At most the complete 256-entry history plus one poll; stay bounded. */
    for(unsigned i=0;i<5;i++){
        uint64_t n=cx_keys(events,64,&key_cursor);
        if(n==CX_ERROR){failed=1;break;}
        if(!n)break;
    }
    memset(&keyboard,0,sizeof(keyboard));
}
static int script(const char *path) {
    size_t path_n=strlen(path);
    uint64_t size=cx_size(path,path_n);
    if(size==CX_ERROR||size>65536)return 0;
    char *data=malloc((size_t)size+1);if(!data)return 0;
    if(cx_read(path,path_n,0,data,(size_t)size)!=size){free(data);return 0;}
    data[size]=0;
    /* Validate framing before running any command. Scripts are LF-terminated. */
    size_t col=0;
    for(size_t i=0;i<size;i++){
        if(!data[i]||(data[i]!='\n'&&++col>=CON_LINE)){free(data);return 0;}
        if(data[i]=='\n')col=0;
    }
    if(size&&data[size-1]!='\n'){free(data);return 0;}
    int ok=1;
    for(size_t off=0;off<size&&!quit;){
        size_t n=0;while(data[off]!='\n')line[n++]=data[off++];
        off++;line[n]=0;line_n=n;
        if(!execute()||!paint()){ok=0;break;}
    }
    free(data);return ok;
}
int main(int argc,char **argv) {
    const char *script_path=0,*log="runtime/console.log",*capture=0;
    if(argc==0||argc==1)interactive=1;
    else if((argc==4||argc==5)&&!strcmp(argv[1],"--script")){
        script_path=argv[2];log=argv[3];if(argc==5)capture=argv[4];
    } else return 120;
    if(!path_ok(log)||(script_path&&!path_ok(script_path))||(capture&&!path_ok(capture)))return 121;
    if(script_path&&!strcmp(script_path,log))return 121;
    if(!init_screen())return 122;
    if(interactive&&cx_input_available()!=1)return 123;
    transcript=fopen(log,interactive?"ab":"wx");
    if(!transcript)return 124;
    output("CodexOS console\n");output("Type help for commands.\n");
    int ok=1;
    if(script_path)ok=script(script_path);
    else {
        discard_keys();
        paint();
        while(!quit&&!failed){
            struct cx_key_event events[32];uint64_t n=cx_keys(events,32,&key_cursor);
            if(n==CX_ERROR){failed=1;break;}
            for(uint64_t i=0;i<n&&!quit&&!failed;i++){
                int c=con_key(&keyboard,events[i].code,events[i].pressed,events[i].flags);
                int edit=con_edit(line,&line_n,&line_overflow,c);
                if(edit){
                    if(edit>0)execute();
                    else {output("error: line too long; command discarded\n");line_n=0;line[0]=0;}
                    line_overflow=0;
                    /* Discard foreground-program keys and remaining old batch. */
                    discard_keys();break;
                }
            }
            if(n)paint();
            if(cx_sleep(1)!=0)failed=1;
        }
    }
    if(!paint())ok=0;
    if(capture&&ok){
        FILE *f=fopen(capture,"wx");
        if(!f)ok=0;
        else {
            size_t bytes=(size_t)width*height*4;
            if(fwrite(pixels,1,bytes,f)!=bytes)ok=0;
            if(fclose(f))ok=0;
        }
    }
    if(!flush()||fclose(transcript))ok=0;
    free(pixels);free(screen.cells);
    if(!ok||failed)return 125;
    cx_exit(exit_status);
}
