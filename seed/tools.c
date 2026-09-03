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
static int utf8(const u8*p,u32 n){u32 i=0;while(i<n){u8 f=p[i++];u32 c,m,r;if(f<=0x7f)continue;if(f>=0xc2&&f<=0xdf)c=f&31,m=0x80,r=1;else if(f>=0xe0&&f<=0xef)c=f&15,m=0x800,r=2;else if(f>=0xf0&&f<=0xf4)c=f&7,m=0x10000,r=3;else return 0;if(r>n-i)return 0;while(r--){u8 q=p[i++];if((q&0xc0)!=0x80)return 0;c=(c<<6)|(q&63);}if(c<m||c>0x10ffff||(c>=0xd800&&c<=0xdfff))return 0;}return 1;}
static int dec(struct bytes b,u32*v){u32 x=0;if(!b.n)return 0;for(u32 i=0;i<b.n;++i){u8 c=b.d[i];if(c<'0'||c>'9'||x>(UINT32_MAX-(c-'0'))/10u)return 0;x=x*10u+c-'0';}*v=x;return 1;}
static int canonical(struct bytes b){if(!b.n||(b.n>1&&b.d[0]=='0'))return 0;for(u32 i=0;i<b.n;++i)if(b.d[i]<'0'||b.d[i]>'9')return 0;return 1;}
static int path(struct bytes b){return b.n&&b.n<=FILE_MAX_PATH_LENGTH&&utf8(b.d,b.n);}
static void header(u32 id,u32 status,u32 n){frame_send_header(INVOKE_TOOL_RESPONSE,id,4u+n);frame_write_u32(status);}void tools_send_failure(u32 id){header(id,1,0);}static void ok(u32 id){header(id,0,0);}static void number(u32 id,u64 v){u8 r[20],b[20];u32 n=0;do{r[n++]=(u8)('0'+v%10u);v/=10u;}while(v);for(u32 i=0;i<n;++i)b[i]=r[n-i-1u];header(id,0,n);serial_write_bytes(b,n);}static u64 lock(void){u64 f;__asm__ volatile("pushfq; popq %0; cli":"=r"(f)::"memory");return f;}static void unlock(u64 f){__asm__ volatile("pushq %0; popfq"::"r"(f):"memory","cc");}
void tools_send_list(u32 id){u32 n=2;for(u32 i=0;i<sizeof(names)/sizeof(names[0]);++i)n+=2u+names[i].n;frame_send_header(LIST_TOOLS_RESPONSE,id,n);frame_write_u16(sizeof(names)/sizeof(names[0]));for(u32 i=0;i<sizeof(names)/sizeof(names[0]);++i){frame_write_u16(names[i].n);serial_write_bytes(names[i].d,names[i].n);}}
static int parse(const u8*p,u32 n,struct invocation*v){u32 o=0;if(n<2)return 0;u16 z=r16(p);o=2;if(!z||z>255||z>n-o)return 0;v->name.d=p+o;v->name.n=z;if(!utf8(v->name.d,z)||n-(o+=z)<2)return 0;v->count=r16(p+o);o+=2;if(v->count>MAX_TOOL_ARGUMENTS)return 0;for(u16 i=0;i<v->count;++i){if(n-o<4)return 0;u32 q=r32(p+o);o+=4;if(q>n-o)return 0;v->a[i].d=p+o;v->a[i].n=q;o+=q;}return o==n;}
void tools_handle_invocation(u32 id,const u8*p,u32 n){struct invocation v;if(!parse(p,n,&v)){tools_send_failure(id);return;}u32 tool=sizeof(names)/sizeof(names[0]);for(u32 i=0;i<tool;++i)if(eq(v.name,names[i])){tool=i;break;}switch(tool){
case 0:{struct bytes prefix={0,0};if(v.count>1||(v.count==1&&!utf8(v.a[0].d,v.a[0].n)))break;if(v.count)prefix=v.a[0];u64 f=lock();u32 out=0;for(u32 i=0;i<file_count;++i)if(file_path_has_prefix(&files[i],prefix.d,prefix.n))out+=files[i].path_length+1u;header(id,0,out);for(u32 i=0;i<file_count;++i)if(file_path_has_prefix(&files[i],prefix.d,prefix.n)){serial_write_bytes(files[i].path,files[i].path_length);serial_write('\n');}unlock(f);return;}
case 1:{u32 off,len;if(v.count!=3||!path(v.a[0])||!dec(v.a[1],&off)||!dec(v.a[2],&len)||len>FRAME_MAX_PAYLOAD-4u)break;u64 q=lock();struct file*f=file_find(v.a[0].d,v.a[0].n);if(!f||off>file_size(f)){unlock(q);break;}u32 left=file_size(f)-off,out=len<left?len:left;header(id,0,out);serial_write_bytes(file_content(f)+off,out);unlock(q);return;}
case 2:{u32 off;if(v.count!=3||!path(v.a[0])||!dec(v.a[1],&off))break;u64 f=lock();int done=file_write(v.a[0].d,v.a[0].n,off,v.a[2].d,v.a[2].n);unlock(f);if(!done)break;ok(id);return;}
case 3:{u32 z;if(v.count!=2||!path(v.a[0])||!dec(v.a[1],&z))break;u64 f=lock();int done=file_truncate(v.a[0].d,v.a[0].n,z);unlock(f);if(!done)break;ok(id);return;}
case 4:{if(v.count!=1||!path(v.a[0]))break;u64 f=lock();int done=file_remove(v.a[0].d,v.a[0].n);unlock(f);if(!done)break;ok(id);return;}
case 5:{if(v.count)break;u64 f=lock();build_tool_invoke(id);unlock(f);return;}
case 6:{if(v.count!=1||v.a[0].n>FINISH_GENERATION_HANDOFF_MAX||!utf8(v.a[0].d,v.a[0].n))break;u64 f=lock();finish_generation_tool_invoke(id,v.a[0].d,v.a[0].n);unlock(f);return;}
case 7:if(v.count!=2||!v.a[0].n||v.a[0].n>FEATURE_REQUEST_TITLE_MAX||v.a[1].n>FEATURE_REQUEST_DESCRIPTION_MAX||!utf8(v.a[0].d,v.a[0].n)||!utf8(v.a[1].d,v.a[1].n))break;request_feature_tool_invoke(id,v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n);return;
case 8:if(v.count)break;list_provided_assets_tool_invoke(id);return;
case 9:if(v.count!=3||!v.a[0].n||!utf8(v.a[0].d,v.a[0].n)||!canonical(v.a[1])||!canonical(v.a[2]))break;read_provided_asset_tool_invoke(id,v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n,v.a[2].d,v.a[2].n);return;
case 10:{if(v.count!=2||!v.a[0].n||!utf8(v.a[0].d,v.a[0].n)||!path(v.a[1]))break;u64 f=lock();int done=import_provided_asset(v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n);unlock(f);if(!done)break;ok(id);return;}
case 11:{if(v.count!=1||!path(v.a[0]))break;u64 f=lock();int task=task_create_user_file(v.a[0].d,v.a[0].n);unlock(f);if(task<0)break;number(id,(u32)task);return;}
case 12:{u32 task;if(v.count!=1||!dec(v.a[0],&task))break;u64 status;int state=task_reap(task,&status);if(state<0)break;if(!state){header(id,0,7);serial_write_bytes((const u8*)"running",7);}else number(id,status);return;}
}tools_send_failure(id);}
