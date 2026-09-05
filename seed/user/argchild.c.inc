#include "cx.h"
int main(int argc,char **argv) {
    if(!argv || argv[argc]) return 61;
    if(cx_sleep(2)!=0) return 62;
    if(argc==0) return 73;
    if(argc==2 && argv[1][0]=='s' && !argv[1][1])
        cx_exit(UINT64_C(0x1234567887654321));
    if(argc==2 && argv[1][0]=='f' && !argv[1][1]) {
        __asm__ volatile("ud2");
        return 69;
    }
    if(argc==1) {
        if(strlen(argv[0])!=4095) return 63;
        for(size_t j=0;j<4095;++j) if(argv[0][j]!='q') return 64;
        argv[0][0]='!';
        return 73;
    }
    if(argc!=32 || memcmp(argv[0],"argchild",9) || argv[1][0] ||
       memcmp(argv[2],"a b\t",5) ||
       (unsigned char)argv[3][0]!=255 || (unsigned char)argv[3][1]!=128 ||
       argv[3][2]) return 65;
    for(int i=4;i<31;++i) if(argv[i][0]!='x' || argv[i][1]) return 66;
    if(strlen(argv[31])!=4023) return 67;
    for(size_t j=0;j<4023;++j) if(argv[31][j]!='q') return 68;
    argv[31][0]='!';
    argv[2][0]='!';
    argv[0]=0;
    return 73;
}
