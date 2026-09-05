/* Host-side packaging utility. No guest kernel code or workload patching. */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#define LIMIT (16u*1024u*1024u)
#define LOW 0x400000ull
#define HIGH 0x3ffef000ull
static uint16_t r16(const unsigned char *p) { return p[0]|(uint16_t)p[1]<<8; }
static uint32_t r32(const unsigned char *p) { return r16(p)|(uint32_t)r16(p+2)<<16; }
static uint64_t r64(const unsigned char *p) { return r32(p)|(uint64_t)r32(p+4)<<32; }
static void w32(unsigned char *p,uint32_t v) { for(int i=0;i<4;i++) p[i]=v>>(8*i); }
static void w64(unsigned char *p,uint64_t v) { w32(p,v); w32(p+4,v>>32); }
static int range(uint64_t off,uint64_t len,size_t n) { return off<=n && len<=n-off; }
struct seg { uint64_t va,mem,off,size; uint32_t flags; };
static const char *package(const unsigned char *d,size_t n,unsigned char **out,size_t *outn) {
    struct seg s[16]; unsigned count=0; uint64_t pages=0,total,entry;
    *out=0; *outn=0;
    if(n<64 || n>LIMIT || memcmp(d,"\177ELF\2\1\1",7) ||
       (d[7]!=0 && d[7]!=3) || d[8] || r16(d+16)!=2 ||
       r16(d+18)!=62 || r32(d+20)!=1 || r32(d+48) ||
       r16(d+52)!=64 || r16(d+54)!=56)
        return "expected little-endian x86-64 static ET_EXEC";
    uint64_t ph=r64(d+32),sh=r64(d+40);
    unsigned pn=r16(d+56),sn=r16(d+60);
    if(!pn || pn>128 || !range(ph,(uint64_t)pn*56,n))
        return "invalid program header table";
    /* Extended numbering, relocations, dynamic linking and TLS have no runtime. */
    if(sh) {
        if(!sn || r16(d+58)!=64 || !range(sh,(uint64_t)sn*64,n))
            return "invalid section table";
        for(unsigned i=0;i<sn;i++) {
            const unsigned char *h=d+sh+64*i;
            uint32_t type=r32(h+4); uint64_t size=r64(h+32);
            if((r64(h+8)&0x400) || ((type==4 || type==9 || type==6) && size))
                return "relocations, dynamic sections and TLS unsupported";
            if(type!=8 && !range(r64(h+24),size,n))
                return "section outside input";
        }
    } else if(sn) return "missing section table";
    entry=r64(d+24);
    for(unsigned i=0;i<pn;i++) {
        const unsigned char *p=d+ph+56*i;
        uint32_t type=r32(p),flags=r32(p+4);
        uint64_t off=r64(p+8),va=r64(p+16),size=r64(p+32),mem=r64(p+40),align=r64(p+48);
        if(type==2 || type==3 || type==7) return "dynamic linking, interpreter and TLS unsupported";
        if(type==0x6474e551 && (flags&1)) return "executable stack unsupported";
        if(type!=1) {
            if(type!=0 && type!=4 && type!=6 && type!=0x6474e550 &&
               type!=0x6474e551 && type!=0x6474e552)
                return "unsupported program header type";
            continue;
        }
        if(size>mem || !range(off,size,n)) return "invalid segment file range";
        if(!mem) continue;
        if(count==16 || (flags&~7u) || !(flags&4) || ((flags&3)==3))
            return "invalid segment count or permissions";
        if((va&4095) || va<LOW || va>=HIGH || mem>HIGH-va ||
           (align>1 && ((align&(align-1)) || ((va-off)&(align-1)))))
            return "invalid segment address or alignment";
        mem=(mem+4095)&~4095ull;
        if(mem>HIGH-va || (pages+=mem/4096)>16384) return "image memory limit";
        for(unsigned j=0;j<count;j++)
            if(va<s[j].va+s[j].mem && s[j].va<va+mem) return "overlapping pages";
        s[count++]=(struct seg){va,mem,off,size,
            ((flags&4)?1u:0u)|((flags&2)?2u:0u)|((flags&1)?4u:0u)};
    }
    int found=0;
    total=24+32*count;
    for(unsigned i=0;i<count;i++) {
        if((s[i].flags&4) && entry>=s[i].va && entry-s[i].va<s[i].size) found=1;
        total+=s[i].size;
    }
    if(!count || !found) return "entry is not file-backed executable code";
    if(total>LIMIT) return "output file limit";
    unsigned char *b=calloc(1,(size_t)total);
    if(!b) return "out of memory";
    memcpy(b,"CXE2",4); w32(b+4,count); w64(b+8,entry);
    uint32_t pos=24+32*count;
    for(unsigned i=0;i<count;i++) {
        unsigned char *p=b+24+32*i;
        w64(p,s[i].va); w64(p+8,s[i].mem); w32(p+16,pos);
        w32(p+20,s[i].size); w32(p+24,s[i].flags);
        memcpy(b+pos,d+s[i].off,(size_t)s[i].size); pos+=s[i].size;
    }
    *out=b; *outn=total; return 0;
}
static void fixture(unsigned char *d) {
    memset(d,0,8193); memcpy(d,"\177ELF\2\1\1",7);
    d[16]=2; d[18]=62; w32(d+20,1); w64(d+24,LOW); w64(d+32,64);
    d[52]=64; d[54]=56; d[56]=2;
    unsigned char *p=d+64;
    w32(p,1); w32(p+4,5); w64(p+8,4096); w64(p+16,LOW);
    w64(p+32,1); w64(p+40,4096); w64(p+48,4096);
    p+=56; w32(p,1); w32(p+4,6); w64(p+8,8192); w64(p+16,LOW+4096);
    w64(p+32,1); w64(p+40,4101); w64(p+48,4096);
    d[4096]=0xc3; d[8192]=0xa5;
}
static int selftest(void) {
    unsigned char d[8193],*b=0; size_t z=0; fixture(d);
    if(package(d,sizeof(d),&b,&z) || z!=90 || memcmp(b,"CXE2",4) ||
       r32(b+4)!=2 || r64(b+8)!=LOW || r64(b+64)!=8192 ||
       r32(b+48)!=5 || r32(b+80)!=3 || b[88]!=0xc3 || b[89]!=0xa5) {
        free(b); return 1;
    }
    free(b);
    for(unsigned i=0;i<18;i++) {
        fixture(d); size_t n=sizeof(d);
        switch(i) {
        case 0: n=63; break;
        case 1: d[4]=1; break;
        case 2: d[16]=3; break;
        case 3: w64(d+32,UINT64_MAX); break;
        case 4: d[56]=129; break;
        case 5: w32(d+64+4,7); break;
        case 6: w64(d+64+8,8193); break;
        case 7: w64(d+64+32,4097); break;
        case 8: w64(d+64+16,LOW+1); break;
        case 9: w64(d+120+16,LOW); break;
        case 10: w64(d+24,LOW+1); break;
        case 11: w32(d+120,7); break;
        case 12: w32(d+120,3); break;
        case 13: w64(d+120+40,HIGH); break;
        case 14: w64(d+120+40,16384ull*4096); break;
        case 15: w64(d+120+48,3); break;
        case 16: w64(d+40,UINT64_MAX); d[60]=1; break;
        case 17: w32(d+120,0x6474e551); w32(d+124,7); break;
        }
        if(!package(d,n,&b,&z)) { free(b); fprintf(stderr,"accepted bad case %u\n",i); return 1; }
    }
    /* Pure BSS segment is retained with a valid empty file range. */
    fixture(d); w64(d+120+32,0);
    if(package(d,sizeof(d),&b,&z) || z!=89 || r32(b+76)!=0) { free(b); return 1; }
    free(b); puts("elf2cxe self-test: PASS"); return 0;
}
int main(int argc,char **argv) {
    if(argc==2 && !strcmp(argv[1],"--self-test")) return selftest();
    if(argc!=3) { fprintf(stderr,"usage: elf2cxe input.elf output.cxe\n"); return 2; }
    FILE *f=fopen(argv[1],"rb");
    if(!f) { perror(argv[1]); return 1; }
    if(fseek(f,0,SEEK_END)) { fclose(f); return 1; }
    long len=ftell(f);
    if(len<0 || len>LIMIT || fseek(f,0,SEEK_SET)) { fclose(f); return 1; }
    unsigned char *d=malloc(len? (size_t)len:1),*b=0; size_t z=0;
    if(!d) { fclose(f); return 1; }
    if(fread(d,1,len,f)!=(size_t)len) { free(d); fclose(f); return 1; }
    fclose(f);
    const char *error=package(d,len,&b,&z); free(d);
    if(error) { fprintf(stderr,"elf2cxe: %s\n",error); return 1; }
    f=fopen(argv[2],"wb");
    if(!f) { perror(argv[2]); free(b); return 1; }
    int ok=fwrite(b,1,z,f)==z; free(b);
    if(fclose(f)) ok=0;
    if(!ok) { remove(argv[2]); return 1; }
    return 0;
}
