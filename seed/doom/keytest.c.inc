#include "keys.h"
#define C(x) do{if(!(x))return __LINE__;}while(0)
int main(void) {
    struct dg_keys state={0};int p;unsigned char k;
    struct cx_key_event e={.code=0x2a,.pressed=1};
    C(dg_apply(&state,&e,&p,&k)&&p&&k==KEY_RSHIFT);
    C(!dg_apply(&state,&e,&p,&k));
    e.code=0x36;C(!dg_apply(&state,&e,&p,&k));
    e.code=0x2a;e.pressed=0;C(!dg_apply(&state,&e,&p,&k));
    e.code=0x36;C(dg_apply(&state,&e,&p,&k)&&!p&&k==KEY_RSHIFT);
    e.code=0x148;e.pressed=1;C(dg_apply(&state,&e,&p,&k)&&p&&k==KEY_UPARROW);
    e.code=0x1d;C(dg_apply(&state,&e,&p,&k)&&k==KEY_RCTRL);
    dg_lost(&state);
    unsigned releases=0;
    while(dg_release(&state,&p,&k)){C(!p);++releases;}
    C(releases==2);
    e.code=0x148;C(dg_apply(&state,&e,&p,&k)&&p&&k==KEY_UPARROW);
    e.pressed=0;C(dg_apply(&state,&e,&p,&k)&&!p);
    e.code=0x145;e.pressed=1;C(dg_apply(&state,&e,&p,&k)&&k==KEY_PAUSE);
    e.pressed=0;C(dg_apply(&state,&e,&p,&k)&&!p);
    e.code=511;e.pressed=1;C(!dg_apply(&state,&e,&p,&k));
    C(dg_map(0x10)=='q'&&dg_map(0x11c)==KEY_ENTER&&dg_map(0x137)==KEY_PRTSCR);
    return 0;
}
