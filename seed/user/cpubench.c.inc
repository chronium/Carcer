#include "cx.h"
/* Ordinary finite CPU benchmark: the measured loop contains no syscall,
 * yielding, polling of kernel time, or workload-dependent behavior. */
int main(void) {
    uint64_t result[4];
    result[0]=cx_ticks();
    volatile uint64_t value=1;
    for(uint64_t i=0;i<2000000000ull;i++)value=value*6364136223846793005ull+1;
    result[1]=cx_ticks();result[2]=2000000000ull;result[3]=value;
    const char *path="runtime/cpu-result";
    return cx_write(path,18,0,result,sizeof(result))==sizeof(result)?0:1;
}
