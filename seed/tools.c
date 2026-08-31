#include "tools.h"
#include "build.h"
#include "files.h"
#include "protocol.h"
#include "serial.h"
#include "tasks.h"
#define MAX_TOOL_ARGUMENTS 3u
struct bytes{const uint8_t*d;uint32_t n;};struct invocation{struct bytes name;uint16_t count;struct bytes a[MAX_TOOL_ARGUMENTS];};
#define N(x) {(const uint8_t*)x,sizeof(x)-1u}
static const struct bytes names[]={N("list"),N("read"),N("write"),N("truncate"),N("remove"),N("build"),N("finish_generation"),N("request_feature"),N("list_provided_assets"),N("read_provided_asset"),N("import_provided_asset"),N("run")};
static int eq(struct bytes a,struct bytes b){if(a.n!=b.n)return 0;for(uint32_t i=0;i<a.n;++i)if(a.d[i]!=b.d[i])return 0;return 1;}
static uint16_t r16(const uint8_t*p){return(uint16_t)p[0]|((uint16_t)p[1]<<8);}static uint32_t r32(const uint8_t*p){return(uint32_t)p[0]|((uint32_t)p[1]<<8)|((uint32_t)p[2]<<16)|((uint32_t)p[3]<<24);}
static int utf8(const uint8_t*p,uint32_t n){uint32_t i=0;while(i<n){uint8_t f=p[i++];uint32_t c,m,r;if(f<=0x7f)continue;if(f>=0xc2&&f<=0xdf)c=f&31,m=0x80,r=1;else if(f>=0xe0&&f<=0xef)c=f&15,m=0x800,r=2;else if(f>=0xf0&&f<=0xf4)c=f&7,m=0x10000,r=3;else return 0;if(r>n-i)return 0;while(r--){uint8_t q=p[i++];if((q&0xc0)!=0x80)return 0;c=(c<<6)|(q&63);}if(c<m||c>0x10ffff||(c>=0xd800&&c<=0xdfff))return 0;}return 1;}
static int dec(struct bytes b,uint32_t*v){uint32_t x=0;if(!b.n)return 0;for(uint32_t i=0;i<b.n;++i){uint8_t c=b.d[i];if(c<'0'||c>'9'||x>(UINT32_MAX-(c-'0'))/10u)return 0;x=x*10u+c-'0';}*v=x;return 1;}
static int canonical(struct bytes b){if(!b.n||(b.n>1&&b.d[0]=='0'))return 0;for(uint32_t i=0;i<b.n;++i)if(b.d[i]<'0'||b.d[i]>'9')return 0;return 1;}
static int path(struct bytes b){return b.n&&b.n<=FILE_MAX_PATH_LENGTH&&utf8(b.d,b.n);}
static void header(uint32_t id,uint32_t status,uint32_t n){frame_send_header(INVOKE_TOOL_RESPONSE,id,4u+n);frame_write_u32(status);}void tools_send_failure(uint32_t id){header(id,1,0);}static void ok(uint32_t id){header(id,0,0);}static void number(uint32_t id,uint32_t v){uint8_t r[10],b[10];uint32_t n=0;do{r[n++]=(uint8_t)('0'+v%10u);v/=10u;}while(v);for(uint32_t i=0;i<n;++i)b[i]=r[n-i-1u];header(id,0,n);serial_write_bytes(b,n);}static uint64_t lock(void){uint64_t f;__asm__ volatile("pushfq; popq %0; cli":"=r"(f)::"memory");return f;}static void unlock(uint64_t f){__asm__ volatile("pushq %0; popfq"::"r"(f):"memory","cc");}
void tools_send_list(uint32_t id){uint32_t n=2;for(uint32_t i=0;i<sizeof(names)/sizeof(names[0]);++i)n+=2u+names[i].n;frame_send_header(LIST_TOOLS_RESPONSE,id,n);frame_write_u16(sizeof(names)/sizeof(names[0]));for(uint32_t i=0;i<sizeof(names)/sizeof(names[0]);++i){frame_write_u16(names[i].n);serial_write_bytes(names[i].d,names[i].n);}}
static int parse(const uint8_t*p,uint32_t n,struct invocation*v){uint32_t o=0;if(n<2)return 0;uint16_t z=r16(p);o=2;if(!z||z>255||z>n-o)return 0;v->name.d=p+o;v->name.n=z;if(!utf8(v->name.d,z)||n-(o+=z)<2)return 0;v->count=r16(p+o);o+=2;if(v->count>MAX_TOOL_ARGUMENTS)return 0;for(uint16_t i=0;i<v->count;++i){if(n-o<4)return 0;uint32_t q=r32(p+o);o+=4;if(q>n-o)return 0;v->a[i].d=p+o;v->a[i].n=q;o+=q;}return o==n;}
void tools_handle_invocation(uint32_t id,const uint8_t*p,uint32_t n){struct invocation v;if(!parse(p,n,&v)){tools_send_failure(id);return;}uint32_t tool=sizeof(names)/sizeof(names[0]);for(uint32_t i=0;i<tool;++i)if(eq(v.name,names[i])){tool=i;break;}switch(tool){
case 0:{struct bytes prefix={0,0};if(v.count>1||(v.count==1&&!utf8(v.a[0].d,v.a[0].n)))break;if(v.count)prefix=v.a[0];uint64_t f=lock();uint32_t out=0;for(uint32_t i=0;i<file_count;++i)if(file_path_has_prefix(&files[i],prefix.d,prefix.n))out+=files[i].path_length+1u;header(id,0,out);for(uint32_t i=0;i<file_count;++i)if(file_path_has_prefix(&files[i],prefix.d,prefix.n)){serial_write_bytes(files[i].path,files[i].path_length);serial_write('\n');}unlock(f);return;}
case 1:{uint32_t off,len;if(v.count!=3||!path(v.a[0])||!dec(v.a[1],&off)||!dec(v.a[2],&len)||len>FRAME_MAX_PAYLOAD-4u)break;uint64_t q=lock();struct file*f=file_find(v.a[0].d,v.a[0].n);if(!f||off>file_size(f)){unlock(q);break;}uint32_t left=file_size(f)-off,out=len<left?len:left;header(id,0,out);serial_write_bytes(file_content(f)+off,out);unlock(q);return;}
case 2:{uint32_t off;if(v.count!=3||!path(v.a[0])||!dec(v.a[1],&off))break;uint64_t f=lock();int done=file_write(v.a[0].d,v.a[0].n,off,v.a[2].d,v.a[2].n);unlock(f);if(!done)break;ok(id);return;}
case 3:{uint32_t z;if(v.count!=2||!path(v.a[0])||!dec(v.a[1],&z))break;uint64_t f=lock();int done=file_truncate(v.a[0].d,v.a[0].n,z);unlock(f);if(!done)break;ok(id);return;}
case 4:{if(v.count!=1||!path(v.a[0]))break;uint64_t f=lock();int done=file_remove(v.a[0].d,v.a[0].n);unlock(f);if(!done)break;ok(id);return;}
case 5:{if(v.count)break;uint64_t f=lock();build_tool_invoke(id);unlock(f);return;}
case 6:{if(v.count!=1||v.a[0].n>FINISH_GENERATION_HANDOFF_MAX||!utf8(v.a[0].d,v.a[0].n))break;uint64_t f=lock();finish_generation_tool_invoke(id,v.a[0].d,v.a[0].n);unlock(f);return;}
case 7:if(v.count!=2||!v.a[0].n||v.a[0].n>FEATURE_REQUEST_TITLE_MAX||v.a[1].n>FEATURE_REQUEST_DESCRIPTION_MAX||!utf8(v.a[0].d,v.a[0].n)||!utf8(v.a[1].d,v.a[1].n))break;request_feature_tool_invoke(id,v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n);return;
case 8:if(v.count)break;list_provided_assets_tool_invoke(id);return;
case 9:if(v.count!=3||!v.a[0].n||!utf8(v.a[0].d,v.a[0].n)||!canonical(v.a[1])||!canonical(v.a[2]))break;read_provided_asset_tool_invoke(id,v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n,v.a[2].d,v.a[2].n);return;
case 10:{if(v.count!=2||!v.a[0].n||!utf8(v.a[0].d,v.a[0].n)||!path(v.a[1]))break;uint64_t f=lock();int done=import_provided_asset(v.a[0].d,v.a[0].n,v.a[1].d,v.a[1].n);unlock(f);if(!done)break;ok(id);return;}
case 11:{if(v.count!=1||!path(v.a[0]))break;uint64_t f=lock();int task=task_create_user_file(v.a[0].d,v.a[0].n);unlock(f);if(task<0)break;number(id,(uint32_t)task);return;}
}tools_send_failure(id);}
