#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include "core.h"
#include "font.h"
#define C(x) do{if(!(x)){fprintf(stderr,"core test line %d\n",__LINE__);return 1;}}while(0)
int main(void) {
    char line[CON_LINE],*a[CON_ARGS];
    strcpy(line,"  run 'a b' \"\" x\\ y \"q\\\"r\" '\\'  ");
    C(con_parse(line,a)==6);
    C(!strcmp(a[0],"run")&&!strcmp(a[1],"a b")&&!strcmp(a[2],"")&&
      !strcmp(a[3],"x y")&&!strcmp(a[4],"q\"r")&&!strcmp(a[5],"\\"));
    strcpy(line,"echo 'unterminated");C(con_parse(line,a)==-1);
    strcpy(line,"echo dangling\\");C(con_parse(line,a)==-1);
    strcpy(line,"ab\"\"cd");C(con_parse(line,a)==1&&!strcmp(a[0],"abcd"));
    memset(line,0,sizeof(line));
    for(unsigned i=0;i<32;i++){line[i*2]='x';line[i*2+1]=' ';}
    C(con_parse(line,a)==32);
    for(unsigned i=0;i<33;i++){line[i*2]='x';line[i*2+1]=' ';}
    line[66]=0;C(con_parse(line,a)==-1);
    size_t length=0;int overflow=0;line[0]=0;
    for(unsigned i=0;i<CON_LINE-1;i++)C(con_edit(line,&length,&overflow,'a')==0);
    C(length==CON_LINE-1&&!overflow&&con_edit(line,&length,&overflow,'\n')==1);
    C(con_edit(line,&length,&overflow,'b')==0&&overflow);
    C(con_edit(line,&length,&overflow,'\b')==0&&con_edit(line,&length,&overflow,'\n')==-1);
    C(con_edit(line,&length,&overflow,3)==0&&!length&&!overflow&&!line[0]);
    C(con_edit(line,&length,&overflow,'x')==0&&con_edit(line,&length,&overflow,'\n')==1&&!strcmp(line,"x"));
    struct con_keyboard k={0};
    C(con_key(&k,30,1,0)=='a');
    C(!con_key(&k,42,1,0)&&con_key(&k,30,1,0)=='A');
    C(!con_key(&k,54,1,0)&&!con_key(&k,42,0,0)&&con_key(&k,2,1,0)=='!');
    C(!con_key(&k,54,0,0)&&!con_key(&k,58,1,0)&&con_key(&k,30,1,0)=='A');
    C(!con_key(&k,58,1,1)&&con_key(&k,30,1,0)=='A');
    C(!con_key(&k,58,0,0)&&!con_key(&k,58,1,0)&&con_key(&k,30,1,0)=='a');
    C(!con_key(&k,0x11d,1,0)&&con_key(&k,46,1,0)==3);
    C(con_key(&k,30,1,2)==3&&con_key(&k,30,1,0)=='a');
    C(con_key(&k,0x11c,1,0)=='\n'&&con_key(&k,14,1,1)=='\b');
    C(!con_key(&k,0x145,1,0)&&!con_key(&k,30,0,0));
    char cells[14];memset(cells,'!',sizeof(cells));
    struct con_screen s={cells+1,4,3,0,0};con_clear(&s);
    const char *text="ab\ncd\nef\ngh";
    while(*text)con_put(&s,(unsigned char)*text++);
    C(!memcmp(cells+1,"cd  ef  gh  ",12)&&cells[0]=='!'&&cells[13]=='!');
    con_clear(&s);text="1234567890123";
    while(*text)con_put(&s,(unsigned char)*text++);
    C(!memcmp(cells+1,"567890123   ",12)&&cells[0]=='!'&&cells[13]=='!');
    C(con_font['A'-32][0]==14&&con_font['a'-32][0]==0);
    for(unsigned i=0;i<95;i++)for(unsigned j=0;j<7;j++)C(con_font[i][j]<32);
    puts("console core PASS");return 0;
}
