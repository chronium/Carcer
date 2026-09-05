#pragma GCC target("general-regs-only")
#include "source_snapshot.h"
#include "files.h"
#include "protocol.h"
#include "serial.h"
static int valid(const struct file*f){static const uint8_t p[]="seed/";uint32_t i,s=sizeof(p)-1u,n=f->n;if(n<=s)return 0;for(i=0;i<s;++i)if(f->p[i]!=p[i])return 0;for(i=0;i<n;++i)if(!f->p[i])return 0;for(i=s;i<=n;++i)if(i==n||f->p[i]=='/'){uint32_t z=i-s;if(!z||(z==1&&f->p[s]=='.')||(z==2&&f->p[s]=='.'&&f->p[s+1u]=='.'))return 0;s=i+1u;}return 1;}int ssmeasure(uint16_t*c,uint32_t*n){uint16_t q=0;uint32_t z=2;for(uint32_t i=0;i<fc;++i){const struct file*f=&files[i];if(!valid(f))continue;uint32_t x=6u+f->n;if(x>FM-z||fz(f)>FM-z-x)return 0;z+=x+fz(f);++q;}*c=q;*n=z;return 1;}void sswrite(uint16_t count){pw16(count);for(uint32_t i=0;i<fc;++i){const struct file*f=&files[i];if(!valid(f))continue;pw16(f->n);swb(f->p,f->n);pw32(fz(f));swb(fd(f),fz(f));}}
