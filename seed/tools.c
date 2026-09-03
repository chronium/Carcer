#include "tools.h"
#include "build.h"
#include "files.h"
#include "protocol.h"
#include "serial.h"
#include "tasks.h"
typedef unsigned char u8;typedef unsigned short u16;typedef unsigned u32;typedef unsigned long u64;
#define MAX_TOOL_ARGUMENTS 3u
struct bytes{const u8*d;u32 n;};struct invocation{struct bytes name;u16 count;struct bytes a[MAX_TOOL_ARGUMENTS];};
#define N(x) {(const u8*)x,sizeof(x)-1u}
static const struct bytes names[]={N("list"),N("read"),N("write"),N("truncate"),N("remove"),N("build"),N("finish_generation"),N("request_feature"),N("list_provided_assets"),N("read_provided_asset"),N("import_provided_asset"),N("run"),N("reap")};
static int eq(struct bytes a,struct bytes b){if(a.n!=b.n)return 0;for(u32 i=0;i<a.n;++i)if(a.d[i]!=b.d[i])return 0;return 1;}
static u16 r16(const u8*p){return(u16)p[0]|((u16)p[1]<<8);}static u32 r32(const u8*p){return(u32)p[0]|((u32)p[1]<<8)|((u32)p[2]<<16)|((u32)p[3]<<24);}
static int dec(struct bytes b,u32*v){u32 x=0;if(!b.n)return 0;for(u32 i=0;i<b.n;++i){u8 c=b.d[i];if(c<'0'||c>'9'||x>(UINT32_MAX-(c-'0'))/10u)return 0;x=x*10u+c-'0';}*v=x;return 1;}
static int canonical(struct bytes b){if(!b.n||(b.n>1&&b.d[0]=='0'))return 0;for(u32 i=0;i<b.n;++i)if(b.d[i]<'0'||b.d[i]>'9')return 0;return 1;}
static int path(struct bytes b){return fpv(b.d,b.n);}
static void header(u32 id,u32 status,u32 n){ph(INVOKE_TOOL_RESPONSE,id,4u+n);pw32(status);}void tlsfail(u32 id){header(id,1,0);}static void ok(u32 id){header(id,0,0);}static void number(u32 id,u64 v){u8 r[20],b[20];u32 n=0;do{r[n++]=(u8)('0'+v%10u);v/=10u;}while(v);for(u32 i=0;i<n;++i)b[i]=r[n-i-1u];header(id,0,n);swb(b,n);}static u64 lock(void){u64 f;__asm__ volatile("pushfq; popq %0; cli":"=r"(f)::"memory");return f;}static void unlock(u64 f){__asm__ volatile("pushq %0; popfq"::"r"(f):"memory","cc");}
void tlslist(u32 id){u32 n=2;for(u32 i=0;i<sizeof(names)/sizeof(names[0]);++i)n+=2u+names[i].n;ph(LIST_TOOLS_RESPONSE,id,n);pw16(sizeof(names)/sizeof(names[0]));for(u32 i=0;i<sizeof(names)/sizeof(names[0]);++i){pw16(names[i].n);swb(names[i].d,names[i].n);}}
static int parse(const u8*p,u32 n,struct invocation*v){u32 o=0;if(n<2)return 0;u16 z=r16(p);o=2;if(!z||z>255||z>n-o)return 0;v->name.d=p+o;v->name.n=z;if(!fu(v->name.d,z)||n-(o+=z)<2)return 0;v->count=r16(p+o);o+=2;if(v->count>MAX_TOOL_ARGUMENTS)return 0;for(u16 i=0;i<v->count;++i){if(n-o<4)return 0;u32 q=r32(p+o);o+=4;if(q>n-o)return 0;v->a[i].d=p+o;v->a[i].n=q;o+=q;}return o==n;}
void tlshandle(u32 id,const u8*p,u32 n){struct invocation v;if(!parse(p,n,&v)){tlsfail(id);return;}u32 tool=sizeof(names)/sizeof(names[0]);for(u32 i=0;i<tool;++i)if(eq(v.name,names[i])){tool=i;break;}switch(tool){
case 0:{struct bytes prefix={0,0};if(v.count>1||(v.count==1&&!fu(v.a[0].d,v.a[0].n)))break;if(v.count)prefix=v.a[0];u64 f=lock();u32 out=0;for(u32 i=0;i<fc;++i)if(fpp(&files[i],prefix.d,prefix.n))out+=files[i].n+1u;header(id,0,out);for(u32 i=0;i<fc;++i)if(fpp(&files[i],prefix.d,prefix.n)){swb(files[i].p,files[i].n);sw('\n');}unlock(f);return;}
case 1:{u32 off,len;if(v.count!=3||!path(v.a[0])||!dec(v.a[1],&off)||!dec(v.a[2],&len)||len>FM-4u)break;u64 q=lock();struct file*f=ff(v.a[0].d,v.a[0].n);if(!f||off>fz(f)){unlock(q);break;}u32 left=fz(f)-off,out=len<left?len:left;header(id,0,out);swb(fd(f)+off,out);unlock(q);return;}
case 2:{u32 off;if(v.count!=3||!path(v.a[0])||!dec(v.a[1],&off))break;u64 f=lock();int done=fw(v.a[0].d,v.a[0].n,off,v.a[2].d,v.a[2].n);unlock(f);if(!done)break;ok(id);return;}
case 3:{u32 z;if(v.count!=2||!path(v.a[0])||!dec(v.a[1],&z))break;u64 f=lock();int done=ft(v.a[0].d,v.a[0].n,z);unlock(f);if(!done)break;ok(id);return;}
case 4:{if(v.count!=1||!path(v.a[0]))break;u64 f=lock();int done=fr(v.a[0].d,v.a[0].n);unlock(f);if(!done)break;ok(id);return;}
case 5:{if(v.count)break;u64 f=lock();tbuild(id);unlock(f);return;}
case 6:{if(v.count!=1||v.a[0].n>HM||!fu(v.a[0].d,v.a[0].n))break;u64 f=lock();tfinish(id,v.a[0].d,v.a[0].n);unlock(f);return;}
case 7:if(v.count!=2||!v.a[0].n||v.a[0].n>QTM||v.a[1].n>QDM||!fu(v.a[0].d,v.a[0].n)||!fu(v.a[1].d,v.a[1].n))break;trequest(id,v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n);return;
case 8:if(v.count)break;tassets(id);return;
case 9:if(v.count!=3||!v.a[0].n||!fu(v.a[0].d,v.a[0].n)||!canonical(v.a[1])||!canonical(v.a[2]))break;tassetread(id,v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n,v.a[2].d,v.a[2].n);return;
case 10:{if(v.count!=2||!v.a[0].n||!fu(v.a[0].d,v.a[0].n)||!path(v.a[1]))break;u64 f=lock();int done=aimp(v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n);unlock(f);if(!done)break;ok(id);return;}
case 11:{if(v.count!=1||!path(v.a[0]))break;u64 f=lock();int task=tfile(v.a[0].d,v.a[0].n);unlock(f);if(task<0)break;number(id,(u32)task);return;}
case 12:{u32 task;if(v.count!=1||!dec(v.a[0],&task))break;u64 status;int state=twait(task,&status);if(state<0)break;if(!state){header(id,0,7);swb((const u8*)"running",7);}else number(id,status);return;}
}tlsfail(id);}
