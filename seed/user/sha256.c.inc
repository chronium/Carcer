#include "cx.h"
struct sha256 {
    uint32_t h[8];
    uint64_t bytes;
    size_t used;
    unsigned char block[64];
};
static uint32_t rotate(uint32_t x,unsigned n) { return (x>>n)|(x<<(32-n)); }
static void compress(struct sha256 *s,const unsigned char *p) {
    static const uint32_t k[64]={
        0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
        0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
        0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
        0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
        0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
        0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
        0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
        0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2};
    uint32_t w[64];
    for(unsigned i=0;i<16;++i)
        w[i]=((uint32_t)p[i*4]<<24)|((uint32_t)p[i*4+1]<<16)|
             ((uint32_t)p[i*4+2]<<8)|p[i*4+3];
    for(unsigned i=16;i<64;++i) {
        uint32_t x=w[i-15],y=w[i-2];
        w[i]=w[i-16]+(rotate(x,7)^rotate(x,18)^(x>>3))+w[i-7]+
             (rotate(y,17)^rotate(y,19)^(y>>10));
    }
    uint32_t a=s->h[0],b=s->h[1],c=s->h[2],d=s->h[3];
    uint32_t e=s->h[4],f=s->h[5],g=s->h[6],h=s->h[7];
    for(unsigned i=0;i<64;++i) {
        uint32_t t=h+(rotate(e,6)^rotate(e,11)^rotate(e,25))+
                   ((e&f)^(~e&g))+k[i]+w[i];
        uint32_t u=(rotate(a,2)^rotate(a,13)^rotate(a,22))+
                   ((a&b)^(a&c)^(b&c));
        h=g;g=f;f=e;e=d+t;d=c;c=b;b=a;a=t+u;
    }
    s->h[0]+=a;s->h[1]+=b;s->h[2]+=c;s->h[3]+=d;
    s->h[4]+=e;s->h[5]+=f;s->h[6]+=g;s->h[7]+=h;
}
static void begin(struct sha256 *s) {
    const uint32_t h[8]={0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,
                       0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19};
    memcpy(s->h,h,sizeof(h));
    s->bytes=0;s->used=0;
}
static void update(struct sha256 *s,const void *data,size_t n) {
    const unsigned char *p=data;
    s->bytes+=n;
    while(n) {
        size_t take=64-s->used;
        if(take>n) take=n;
        memcpy(s->block+s->used,p,take);
        s->used+=take;p+=take;n-=take;
        if(s->used==64) { compress(s,s->block);s->used=0; }
    }
}
static void finish(struct sha256 *s,char out[65]) {
    uint64_t bits=s->bytes*8;
    unsigned char pad[64]={0x80};
    size_t count=s->used<56?56-s->used:120-s->used;
    update(s,pad,count);
    unsigned char end[8];
    for(unsigned i=0;i<8;++i) end[i]=(unsigned char)(bits>>(56-i*8));
    update(s,end,8);
    const char *hex="0123456789abcdef";
    for(unsigned i=0;i<64;++i) out[i]=hex[(s->h[i/8]>>(28-(i%8)*4))&15];
    out[64]='\n';
}
#ifdef SHA256_SELFTEST
/* Host-only known-answer tests use exactly the user utility's hash code. */
int main(void) {
    struct sha256 s;char out[65];
    begin(&s);finish(&s,out);
    if(memcmp(out,"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n",65)) return 1;
    begin(&s);update(&s,"a",1);update(&s,"bc",2);finish(&s,out);
    if(memcmp(out,"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad\n",65)) return 2;
    begin(&s);
    for(unsigned i=0;i<1000000;++i) update(&s,"a",1);
    finish(&s,out);
    if(memcmp(out,"cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0\n",65)) return 3;
    static const unsigned sizes[4]={55,56,63,64};
    static const char expected[4][66]={
        "9f4390f8d30c2dd92ec9f095b65e2b9ae9b0a925a5258e241c9f1e910f734318\n",
        "b35439a4ac6f0948b6d6f9e3c6af0f5f590ce20f1bde7090ef7970686ec6738a\n",
        "7d3e74a05d7db15bce4ad9ec0658ea98e3f06eeecf16b4c6fff2da457ddc2f34\n",
        "ffe054fe7ae0cb6dc65c3af9b61d5209f439851db43d0ba5997337df154668eb\n"};
    for(unsigned pass=0;pass<4;++pass) {
        begin(&s);
        for(unsigned i=0;i<sizes[pass];++i) update(&s,"a",1);
        finish(&s,out);
        if(memcmp(out,expected[pass],65)) return 4+(int)pass;
    }
    return 0;
}
#else
/* Usage: sha256 input-path new-output-path. Output is 64 hex digits + LF.
 * Files may change between syscalls; immutable inputs give a stable digest.
 * Existing destinations are rejected at the initial check, not reserved. */
int main(int argc,char **argv) {
    if(argc!=3) return 110;
    size_t inlen=strlen(argv[1]),outlen=strlen(argv[2]);
    if(!inlen || !outlen || inlen>255 || outlen>255) return 110;
    if(cx_attributes(argv[2],outlen)!=CX_ERROR) return 111;
    uint64_t size=cx_size(argv[1],inlen);
    if(size==CX_ERROR) return 112;
    struct sha256 s;begin(&s);
    unsigned char buffer[4096];
    for(uint64_t off=0;off<size;) {
        size_t n=size-off>sizeof(buffer)?sizeof(buffer):(size_t)(size-off);
        if(cx_read(argv[1],inlen,off,buffer,n)!=n) return 113;
        update(&s,buffer,n);off+=n;
    }
    if(cx_size(argv[1],inlen)!=size) return 113;
    char result[65];finish(&s,result);
    return cx_write(argv[2],outlen,0,result,sizeof(result))==sizeof(result)?0:114;
}
#endif
