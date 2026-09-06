#ifndef CONSOLE_CORE_H
#define CONSOLE_CORE_H
#include <stdint.h>
#include <stddef.h>
#include <string.h>
#define CON_LINE 1024
#define CON_ARGS 32
/* In-place bounded lexer. Spaces/tabs separate; single/double quotes and
 * backslash escaping preserve literal bytes. No expansion or operators. */
static int con_parse(char *line,char **args) {
    char *read=line,*write=line;int argc=0;
    while(*read) {
        while(*read==' '||*read=='\t')read++;
        if(!*read)break;
        if(argc==CON_ARGS)return -1;
        args[argc++]=write;
        char quote=0;
        while(*read) {
            char c=*read++;
            if(c=='\\'&&quote!='\'') {if(!*read)return -1;*write++=*read++;continue;}
            if(quote) {if(c==quote)quote=0;else *write++=c;continue;}
            if(c=='\''||c=='"'){quote=c;continue;}
            if(c==' '||c=='\t')break;
            *write++=c;
        }
        if(quote)return -1;
        *write++=0;
    }
    return argc;
}
/* Return 1 for a complete line, -1 for a rejected overlong line.
 * Overflow remains sticky through backspace: discarded bytes cannot be undone.
 * Ctrl-C/Escape clears both the text and the error state. */
static int con_edit(char *line,size_t *length,int *overflow,int c) {
    if(c==3){*length=0;*overflow=0;line[0]=0;}
    else if(c=='\n')return *overflow?-1:1;
    else if(c=='\b'){if(*length)line[--*length]=0;}
    else if(c>=32&&c<127){
        if(*length<CON_LINE-1){line[(*length)++]=(char)c;line[*length]=0;}
        else *overflow=1;
    }
    return 0;
}
struct con_keyboard {unsigned char shift[2],control[2],caps,caps_down;};
/* ASCII editing events: newline submits, backspace erases, 3 clears the line.
 * Event loss clears modifier state and cancels the partially typed command. */
static int con_key(struct con_keyboard *s,unsigned code,int pressed,unsigned flags) {
    if(flags&2){memset(s,0,sizeof(*s));return 3;}
    if(code==0x2a||code==0x36){s->shift[code==0x36]=(unsigned char)pressed;return 0;}
    if(code==0x1d||code==0x11d){s->control[code==0x11d]=(unsigned char)pressed;return 0;}
    if(code==0x3a){if(pressed&&!s->caps_down)s->caps^=1;s->caps_down=(unsigned char)pressed;return 0;}
    if(!pressed)return 0;
    if(code==0x1c||code==0x11c)return '\n';
    if(code==0x0e)return '\b';
    if(code==0x01)return 3;
    static const char plain[128]={
        [2]='1',[3]='2',[4]='3',[5]='4',[6]='5',[7]='6',[8]='7',[9]='8',[10]='9',[11]='0',
        [12]='-',[13]='=',[16]='q',[17]='w',[18]='e',[19]='r',[20]='t',[21]='y',
        [22]='u',[23]='i',[24]='o',[25]='p',[26]='[',[27]=']',
        [30]='a',[31]='s',[32]='d',[33]='f',[34]='g',[35]='h',[36]='j',[37]='k',
        [38]='l',[39]=';',[40]='\'',[41]='`',[43]='\\',
        [44]='z',[45]='x',[46]='c',[47]='v',[48]='b',[49]='n',[50]='m',
        [51]=',',[52]='.',[53]='/',[57]=' '
    };
    static const char shifted[128]={
        [2]='!',[3]='@',[4]='#',[5]='$',[6]='%',[7]='^',[8]='&',[9]='*',[10]='(',[11]=')',
        [12]='_',[13]='+',[26]='{',[27]='}',[39]=':',[40]='"',[41]='~',[43]='|',
        [51]='<',[52]='>',[53]='?'
    };
    if(code>=128)return 0;
    unsigned char c=plain[code];int shift=s->shift[0]||s->shift[1];
    if(s->control[0]||s->control[1])return c=='c'||c=='u'?3:0;
    if(c>='a'&&c<='z'){if(shift!=s->caps)c-=32;}
    else if(shift&&shifted[code])c=shifted[code];
    return c;
}
struct con_screen {char *cells;unsigned cols,rows,x,y;};
static void con_clear(struct con_screen *s){memset(s->cells,' ',s->cols*s->rows);s->x=s->y=0;}
static void con_newline(struct con_screen *s) {
    s->x=0;
    if(++s->y==s->rows){
        memmove(s->cells,s->cells+s->cols,s->cols*(s->rows-1));
        memset(s->cells+s->cols*(s->rows-1),' ',s->cols);s->y--;
    }
}
static void con_put(struct con_screen *s,unsigned char c) {
    if(c=='\n'){con_newline(s);return;}
    if(c<' '||c>126)c='?';
    s->cells[s->y*s->cols+s->x]=(char)c;
    if(++s->x==s->cols)con_newline(s);
}
#endif
