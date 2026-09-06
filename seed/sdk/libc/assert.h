#include "cxlib.h"
#ifdef NDEBUG
#define assert(x) ((void)0)
#else
#define assert(x) ((x)?(void)0:abort())
#endif
