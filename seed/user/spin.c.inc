#include "cx.h"
/* Test observer contract: .probe is first in the writable segment.
 * This is an ordinary volatile object, private to this user address space. */
volatile uint64_t counter __attribute__((section(".probe"))) = 0;
int main(void) { for(;;) ++counter; }
